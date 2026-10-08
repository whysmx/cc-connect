// Package wecomkf implements the "wecom_kf" platform: WeCom (企业微信)
// WeChat Customer Service (微信客服). Personal WeChat users talk to a
// customer-service account; cc-connect pulls their messages via kf/sync_msg,
// hands them to the project's agent and answers through kf/send_msg.
//
// One platform instance serves exactly one customer-service account
// (open_kfid) and therefore one [[projects]] entry. Several projects share a
// single callback listener, see hub.go.
package wecomkf

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

const (
	platformName        = "wecom_kf"
	defaultListenAddr   = "127.0.0.1:8081"
	defaultCallbackPath = "/wecom-kf/callback"
	// maxTextBytes keeps each chunk under the 2048-byte send_msg text limit
	// with room for a truncation marker.
	maxTextBytes = 2000
	// defaultMaxReplies mirrors WeChat Customer Service's rule that at most 5
	// messages may be sent after each customer message.
	defaultMaxReplies = 5
	syncPageLimit     = 1000
	maxSyncPages      = 50
	sendAttempts      = 3
	truncationMarker  = "\n…"
)

func init() {
	core.RegisterPlatform(platformName, New)
}

type replyContext struct {
	openKfID       string
	externalUserID string
	msgID          string
}

// Platform is one WeChat Customer Service account bound to one project.
type Platform struct {
	project       string
	corpID        string
	openKfID      string
	callbackToken string
	aesKey        []byte
	listenAddr    string
	callbackPath  string
	allowFrom     string
	takeoverCheck bool
	maxReplies    int
	// skipBacklog is true when no cursor was persisted yet: the first pull
	// returns up to 3 days of history, which must not be answered.
	skipBacklog bool

	api   *apiClient
	store *stateStore

	handler core.MessageHandler
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	syncMu       sync.Mutex
	syncing      bool
	syncPending  bool
	pendingToken string

	budgetMu sync.Mutex
	budget   map[string]int // external_userid -> messages still allowed
}

// New creates a wecom_kf platform from [[projects.platforms]] options.
func New(opts map[string]any) (core.Platform, error) {
	str := func(key string) string {
		v, _ := opts[key].(string)
		return strings.TrimSpace(v)
	}
	corpID, corpSecret, openKfID := str("corp_id"), str("corp_secret"), str("open_kfid")
	callbackToken, callbackAESKey := str("callback_token"), str("callback_aes_key")
	if corpID == "" || corpSecret == "" || openKfID == "" {
		return nil, fmt.Errorf("wecom_kf: corp_id, corp_secret and open_kfid are required")
	}
	if callbackToken == "" || callbackAESKey == "" {
		return nil, fmt.Errorf("wecom_kf: callback_token and callback_aes_key are required")
	}
	aesKey, err := decodeAESKey(callbackAESKey)
	if err != nil {
		return nil, err
	}

	listenAddr := str("listen_addr")
	if listenAddr == "" {
		listenAddr = defaultListenAddr
	}
	callbackPath := str("callback_path")
	if callbackPath == "" {
		callbackPath = defaultCallbackPath
	}
	if !strings.HasPrefix(callbackPath, "/") {
		callbackPath = "/" + callbackPath
	}

	apiBaseURL := str("api_base_url")
	if apiBaseURL != "" {
		u, err := url.Parse(apiBaseURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return nil, fmt.Errorf("wecom_kf: invalid api_base_url %q", apiBaseURL)
		}
	}
	transport := &http.Transport{MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second}
	if proxy := str("proxy"); proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("wecom_kf: invalid proxy URL: %w", err)
		}
		transport.Proxy = http.ProxyURL(u)
	}
	httpClient := &http.Client{Timeout: 30 * time.Second, Transport: transport}

	takeoverCheck := true
	if v, ok := opts["takeover_check"].(bool); ok {
		takeoverCheck = v
	}
	maxReplies := defaultMaxReplies
	switch v := opts["max_replies_per_message"].(type) {
	case int64:
		maxReplies = int(v)
	case int:
		maxReplies = v
	case float64:
		maxReplies = int(v)
	}
	if maxReplies <= 0 {
		maxReplies = defaultMaxReplies
	}

	dataDir, _ := opts["cc_data_dir"].(string)
	project, _ := opts["cc_project"].(string)
	store, err := newStateStore(stateFilePath(dataDir, corpID, openKfID))
	if err != nil {
		return nil, err
	}

	allowFrom := str("allow_from")
	core.CheckAllowFrom(platformName, allowFrom)

	return &Platform{
		project:       project,
		corpID:        corpID,
		openKfID:      openKfID,
		callbackToken: callbackToken,
		aesKey:        aesKey,
		listenAddr:    listenAddr,
		callbackPath:  callbackPath,
		allowFrom:     allowFrom,
		takeoverCheck: takeoverCheck,
		maxReplies:    maxReplies,
		api:           newAPIClient(apiBaseURL, corpID, corpSecret, httpClient),
		store:         store,
		skipBacklog:   store.Cursor() == "",
		budget:        make(map[string]int),
	}, nil
}

func (p *Platform) Name() string { return platformName }

// Start registers this account on the shared callback listener.
func (p *Platform) Start(handler core.MessageHandler) error {
	p.handler = handler
	p.ctx, p.cancel = context.WithCancel(context.Background())
	if err := registerPlatform(p); err != nil {
		p.cancel()
		return err
	}
	slog.Info("wecom_kf: customer service account ready",
		"project", p.project, "open_kfid", p.openKfID,
		"listen_addr", p.listenAddr, "callback_path", p.callbackPath,
		"takeover_check", p.takeoverCheck)
	return nil
}

// Stop detaches the account and waits for an in-flight pull to finish.
func (p *Platform) Stop() error {
	err := unregisterPlatform(p)
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
	return err
}

// SessionKey builds the per-customer session key
// wecom_kf:{corp_id}:{open_kfid}:{external_userid}. Customers of the same
// account share the project's files but never a conversation, and the same
// customer gets separate sessions on different accounts.
func SessionKey(corpID, openKfID, externalUserID string) string {
	return platformName + ":" + corpID + ":" + openKfID + ":" + externalUserID
}

// ReconstructReplyCtx implements core.ReplyContextReconstructor.
func (p *Platform) ReconstructReplyCtx(sessionKey string) (any, error) {
	parts := strings.SplitN(sessionKey, ":", 4)
	if len(parts) != 4 || parts[0] != platformName || parts[3] == "" {
		return nil, fmt.Errorf("wecom_kf: invalid session key %q", sessionKey)
	}
	if parts[1] != p.corpID || parts[2] != p.openKfID {
		return nil, fmt.Errorf("wecom_kf: session key %q belongs to another customer service account", sessionKey)
	}
	return replyContext{openKfID: parts[2], externalUserID: parts[3]}, nil
}

// FormattingInstructions implements core.FormattingInstructionProvider.
func (p *Platform) FormattingInstructions() string {
	return "You are answering a customer in WeChat Customer Service (微信客服). " +
		"WeChat shows plain text only: do NOT use Markdown (no headings, tables, bold, links or code fences). " +
		"Keep answers short and self-contained, and answer in the customer's language. " +
		"You are in read-only Q&A mode: answer questions from the project materials and never modify files or run write operations."
}

// triggerSync schedules a kf/sync_msg pull. Notifications that arrive while
// a pull is running are coalesced into one follow-up pull so the cursor is
// never used concurrently.
func (p *Platform) triggerSync(token string) {
	p.syncMu.Lock()
	if token != "" {
		p.pendingToken = token
	}
	if p.syncing {
		p.syncPending = true
		p.syncMu.Unlock()
		return
	}
	p.syncing = true
	p.syncMu.Unlock()

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			p.syncMu.Lock()
			tok := p.pendingToken
			p.pendingToken = ""
			p.syncPending = false
			p.syncMu.Unlock()

			p.syncOnce(p.ctx, tok)

			p.syncMu.Lock()
			if !p.syncPending || p.ctx.Err() != nil {
				p.syncing = false
				p.syncMu.Unlock()
				return
			}
			p.syncMu.Unlock()
		}
	}()
}

// syncOnce drains all pending pages for this account.
func (p *Platform) syncOnce(ctx context.Context, token string) {
	for page := 0; page < maxSyncPages; page++ {
		if ctx.Err() != nil {
			return
		}
		resp, err := p.api.syncMsg(ctx, syncMsgRequest{
			Cursor:   p.store.Cursor(),
			Token:    token,
			Limit:    syncPageLimit,
			OpenKfID: p.openKfID,
		})
		if err != nil {
			slog.Error("wecom_kf: sync_msg failed", "project", p.project, "open_kfid", p.openKfID, "error", err)
			return
		}
		for i := range resp.MsgList {
			p.processMessage(ctx, &resp.MsgList[i])
		}
		if resp.NextCursor != "" {
			p.store.SetCursor(resp.NextCursor)
		}
		if err := p.store.Save(); err != nil {
			slog.Warn("wecom_kf: persist sync state failed", "error", err)
		}
		if resp.HasMore == 0 {
			return
		}
	}
	slog.Warn("wecom_kf: sync_msg page limit reached; remaining messages are pulled on the next notification", "open_kfid", p.openKfID)
}

// processMessage filters one pulled message and dispatches customer text to
// the engine.
func (p *Platform) processMessage(ctx context.Context, m *kfMessage) {
	if p.store.MarkSeen(m.MsgID) {
		slog.Debug("wecom_kf: dropping duplicate message", "msgid", m.MsgID)
		return
	}
	switch m.Origin {
	case originCustomer:
	case originSystem:
		p.logEvent(m)
		return
	case originServicer:
		// Human servicer replies (and anything not from the customer) must
		// never be fed back to the agent.
		slog.Debug("wecom_kf: servicer message observed", "open_kfid", m.OpenKfID, "servicer", m.ServicerUserID)
		return
	default:
		return
	}
	if m.OpenKfID != "" && m.OpenKfID != p.openKfID {
		slog.Warn("wecom_kf: message for another account ignored", "want", p.openKfID, "got", m.OpenKfID)
		return
	}
	if m.ExternalUserID == "" {
		return
	}
	if p.skipBacklog && m.SendTime > 0 && core.IsOldMessage(time.Unix(m.SendTime, 0)) {
		slog.Debug("wecom_kf: ignoring history sent before the first startup", "msgid", m.MsgID)
		return
	}
	if !core.AllowList(p.allowFrom, m.ExternalUserID) {
		slog.Warn("wecom_kf: customer rejected by allow_from", "external_userid", m.ExternalUserID)
		return
	}
	if m.MsgType != "text" || m.Text == nil || strings.TrimSpace(m.Text.Content) == "" {
		slog.Info("wecom_kf: unsupported customer message type ignored", "msgtype", m.MsgType, "external_userid", m.ExternalUserID)
		return
	}
	if !p.aiAllowed(ctx, m.ExternalUserID, "dispatch") {
		return
	}

	p.budgetMu.Lock()
	p.budget[m.ExternalUserID] = p.maxReplies
	p.budgetMu.Unlock()

	msg := &core.Message{
		SessionKey:        SessionKey(p.corpID, p.openKfID, m.ExternalUserID),
		Platform:          platformName,
		MessageID:         m.MsgID,
		UserID:            m.ExternalUserID,
		UserName:          m.ExternalUserID,
		Content:           m.Text.Content,
		ReplyCtx:          replyContext{openKfID: p.openKfID, externalUserID: m.ExternalUserID, msgID: m.MsgID},
		UserMessageTimeMs: m.SendTime * 1000,
	}
	slog.Info("wecom_kf: customer message dispatched", "project", p.project, "open_kfid", p.openKfID,
		"external_userid", m.ExternalUserID, "msgid", m.MsgID, "text_len", len(m.Text.Content))
	go p.handler(p, msg)
}

func (p *Platform) logEvent(m *kfMessage) {
	if m.Event == nil {
		return
	}
	ev := m.Event
	switch ev.EventType {
	case "session_status_change":
		slog.Info("wecom_kf: session status changed", "open_kfid", ev.OpenKfID, "external_userid", ev.ExternalUserID,
			"change_type", ev.ChangeType, "old_servicer", ev.OldServicer, "new_servicer", ev.NewServicer)
	case "msg_send_fail":
		slog.Warn("wecom_kf: message delivery failed", "open_kfid", ev.OpenKfID, "external_userid", ev.ExternalUserID,
			"fail_msgid", ev.FailMsgID, "fail_type", ev.FailType)
	default:
		slog.Debug("wecom_kf: event", "type", ev.EventType, "open_kfid", ev.OpenKfID)
	}
}

// aiAllowed checks the WeChat Customer Service session state. When a human
// servicer has taken over (queued or in human service) the AI must stay
// silent. If the state cannot be read the check fails open and logs, so a
// missing permission does not silently break Q&A.
func (p *Platform) aiAllowed(ctx context.Context, externalUserID, stage string) bool {
	if !p.takeoverCheck {
		return true
	}
	state, servicer, err := p.api.serviceState(ctx, p.openKfID, externalUserID)
	if err != nil {
		slog.Warn("wecom_kf: service_state/get failed; continuing without takeover check",
			"stage", stage, "external_userid", externalUserID, "error", err)
		return true
	}
	if aiMayReply(state) {
		return true
	}
	slog.Info("wecom_kf: AI suppressed, session is handled by humans or closed",
		"stage", stage, "external_userid", externalUserID, "state", describeState(state), "servicer", servicer)
	return false
}

func describeState(state int) string {
	switch state {
	case stateUnhandled:
		return "unhandled"
	case stateAssistant:
		return "assistant"
	case stateQueued:
		return "queued_for_human"
	case stateHuman:
		return "human"
	case stateEndedOrNew:
		return "ended"
	}
	return fmt.Sprintf("unknown(%d)", state)
}

// Reply sends content to the customer as plain text. It re-checks the
// session state first so a late AI answer never races a human servicer, and
// it respects the per-customer-message send budget.
func (p *Platform) Reply(ctx context.Context, rctx any, content string) error {
	rc, ok := rctx.(replyContext)
	if !ok {
		return fmt.Errorf("wecom_kf: invalid reply context type %T", rctx)
	}
	content = strings.TrimSpace(core.StripMarkdown(content))
	if content == "" {
		return nil
	}
	if !p.aiAllowed(ctx, rc.externalUserID, "reply") {
		return nil
	}
	chunks := splitText(content, maxTextBytes)
	allowed := p.takeBudget(rc.externalUserID, len(chunks))
	if allowed == 0 {
		slog.Warn("wecom_kf: reply dropped, per-message send limit reached", "external_userid", rc.externalUserID, "limit", p.maxReplies)
		return nil
	}
	if allowed < len(chunks) {
		chunks = chunks[:allowed]
		chunks[allowed-1] += truncationMarker
		slog.Warn("wecom_kf: reply truncated to fit per-message send limit", "external_userid", rc.externalUserID, "limit", p.maxReplies)
	}
	for i, chunk := range chunks {
		if err := p.sendWithRetry(ctx, rc, chunk); err != nil {
			return fmt.Errorf("wecom_kf: send chunk %d/%d: %w", i+1, len(chunks), err)
		}
	}
	return nil
}

// Send implements core.Platform; WeChat Customer Service has no separate
// proactive channel, so it behaves like Reply.
func (p *Platform) Send(ctx context.Context, rctx any, content string) error {
	return p.Reply(ctx, rctx, content)
}

// takeBudget reserves up to n sends for a customer and returns how many were
// granted. Customers without a budget entry (e.g. cron pushes) get the
// default limit.
func (p *Platform) takeBudget(externalUserID string, n int) int {
	p.budgetMu.Lock()
	defer p.budgetMu.Unlock()
	left, ok := p.budget[externalUserID]
	if !ok {
		left = p.maxReplies
	}
	if n > left {
		n = left
	}
	p.budget[externalUserID] = left - n
	return n
}

func (p *Platform) sendWithRetry(ctx context.Context, rc replyContext, text string) error {
	var err error
	for attempt := 1; attempt <= sendAttempts; attempt++ {
		_, err = p.api.sendText(ctx, rc.openKfID, rc.externalUserID, text)
		if err == nil || !isRetryable(err) || attempt == sendAttempts {
			break
		}
		slog.Warn("wecom_kf: send_msg failed, retrying", "attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryDelay(attempt)):
		}
	}
	return err
}

var retryDelay = func(attempt int) time.Duration {
	return time.Duration(attempt) * 500 * time.Millisecond
}

// splitText splits UTF-8 text into chunks of at most maxBytes without
// cutting a rune, preferring to break at a newline.
func splitText(text string, maxBytes int) []string {
	var chunks []string
	for len(text) > maxBytes {
		cut := maxBytes
		for cut > 0 && !isRuneStart(text[cut]) {
			cut--
		}
		if nl := strings.LastIndexByte(text[:cut], '\n'); nl > cut/2 {
			cut = nl + 1
		}
		if cut == 0 {
			cut = maxBytes
		}
		chunks = append(chunks, text[:cut])
		text = text[cut:]
	}
	if text != "" {
		chunks = append(chunks, text)
	}
	return chunks
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

var (
	_ core.Platform                      = (*Platform)(nil)
	_ core.ReplyContextReconstructor     = (*Platform)(nil)
	_ core.FormattingInstructionProvider = (*Platform)(nil)
)
