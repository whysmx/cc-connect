package wecom_kf

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"github.com/chenhg5/cc-connect/platform/wecomkf"
)

func init() { core.RegisterPlatform("wecom-kf", New) }

type encryptedEnvelope struct {
	XMLName xml.Name `xml:"xml"`
	Encrypt string   `xml:"Encrypt"`
}

type replyContext struct {
	toUser  string
	openKfID string
	msgID   string
}

type tokenCache struct {
	mu        sync.Mutex
	value     string
	expiresAt time.Time
}

type Platform struct {
	corpID       string
	corpSecret   string
	callbackToken string
	aesKey       []byte
	openKfID     string
	apiBaseURL   string
	callbackPath string
	listenAddr   string
	allowFrom    string
	client       *wecomkf.Client
	httpClient   *http.Client
	server       *http.Server
	handler      core.MessageHandler

	mu     sync.Mutex
	cursor string
	seen   map[string]time.Time
	token  tokenCache
}

func New(opts map[string]any) (core.Platform, error) {
	corpID, _ := opts["corp_id"].(string)
	secret, _ := opts["corp_secret"].(string)
	callbackToken, _ := opts["callback_token"].(string)
	aesEncoded, _ := opts["callback_aes_key"].(string)
	openKfID, _ := opts["open_kfid"].(string)
	if corpID == "" || secret == "" || callbackToken == "" || aesEncoded == "" || openKfID == "" {
		return nil, fmt.Errorf("wecom-kf: corp_id, corp_secret, callback_token, callback_aes_key and open_kfid are required")
	}
	aesKey, err := wecomkf.DecodeAESKey(aesEncoded)
	if err != nil {
		return nil, err
	}
	apiBaseURL, _ := opts["api_base_url"].(string)
	if strings.TrimSpace(apiBaseURL) == "" {
		apiBaseURL = "https://qyapi.weixin.qq.com"
	}
	callbackPath, _ := opts["callback_path"].(string)
	if callbackPath == "" {
		callbackPath = "/wecom-kf/callback"
	}
	listenAddr, _ := opts["listen_addr"].(string)
	if listenAddr == "" {
		port, _ := opts["port"].(string)
		if port == "" {
			port = "8081"
		}
		listenAddr = ":" + port
	}
	allowFrom, _ := opts["allow_from"].(string)
	core.CheckAllowFrom("wecom-kf", allowFrom)
	httpClient := &http.Client{Timeout: 15 * time.Second}
	return &Platform{
		corpID: corpID, corpSecret: secret, callbackToken: callbackToken,
		aesKey: aesKey, openKfID: openKfID, apiBaseURL: strings.TrimRight(apiBaseURL, "/"),
		callbackPath: callbackPath, listenAddr: listenAddr, allowFrom: allowFrom,
		client: wecomkf.NewClient(apiBaseURL, httpClient), httpClient: httpClient,
		seen: make(map[string]time.Time),
	}, nil
}

func (p *Platform) Name() string { return "wecom-kf" }

func (p *Platform) Start(handler core.MessageHandler) error {
	p.handler = handler
	mux := http.NewServeMux()
	mux.HandleFunc(p.callbackPath, p.callbackHandler)
	p.server = &http.Server{Addr: p.listenAddr, Handler: mux}
	go func() {
		if err := p.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("wecom-kf: callback server failed", "error", err)
		}
	}()
	return nil
}

func (p *Platform) Stop() error {
	if p.server == nil {
		return nil
	}
	return p.server.Shutdown(context.Background())
}

func (p *Platform) callbackHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	signature, timestamp, nonce := q.Get("msg_signature"), q.Get("timestamp"), q.Get("nonce")
	if r.Method == http.MethodGet {
		p.handleVerify(w, signature, timestamp, nonce, q.Get("echostr"))
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || len(body) == 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var envelope encryptedEnvelope
	if err := xml.Unmarshal(body, &envelope); err != nil || envelope.Encrypt == "" ||
		!wecomkf.VerifySignature(p.callbackToken, timestamp, nonce, envelope.Encrypt, signature) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	plain, err := wecomkf.Decrypt(envelope.Encrypt, p.aesKey, p.corpID)
	if err != nil {
		http.Error(w, "bad encrypted payload", http.StatusBadRequest)
		return
	}
	event, err := wecomkf.ParseEvent([]byte(plain))
	if err != nil || event.OpenKfID != p.openKfID {
		http.Error(w, "bad event", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
	go p.pullMessages(event.Token, event.OpenKfID)
}

func (p *Platform) handleVerify(w http.ResponseWriter, signature, timestamp, nonce, echo string) {
	if echo == "" || !wecomkf.VerifySignature(p.callbackToken, timestamp, nonce, echo, signature) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	plain, err := wecomkf.Decrypt(echo, p.aesKey, p.corpID)
	if err != nil {
		http.Error(w, "bad encrypted payload", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(plain))
}

func (p *Platform) pullMessages(pullToken, openKfID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	accessToken, err := p.accessToken(ctx)
	if err != nil {
		slog.Error("wecom-kf: get access token failed", "error", err)
		return
	}
	p.mu.Lock()
	cursor := p.cursor
	p.mu.Unlock()
	for {
		res, err := p.client.SyncMessages(ctx, accessToken, wecomkf.SyncMessageRequest{Cursor: cursor, Token: pullToken, OpenKfID: openKfID, Limit: 1000})
		if err != nil {
			slog.Error("wecom-kf: sync messages failed", "error", err)
			return
		}
		for _, msg := range res.MsgList {
			if msg.Origin != 3 || msg.MsgType != "text" || msg.Text == nil || msg.ExternalUserID == "" {
				continue
			}
			if p.duplicate(msg.MsgID) || !core.AllowList(p.allowFrom, msg.ExternalUserID) {
				continue
			}
			rc := replyContext{toUser: msg.ExternalUserID, openKfID: msg.OpenKfID, msgID: msg.MsgID}
			if p.handler != nil {
				p.handler(p, &core.Message{
					SessionKey: fmt.Sprintf("wecom-kf:%s:%s", msg.OpenKfID, msg.ExternalUserID),
					Platform: p.Name(), MessageID: msg.MsgID, UserID: msg.ExternalUserID,
					Content: msg.Text.Content, ReplyCtx: rc, UserMessageTimeMs: msg.SendTime * 1000,
				})
			}
		}
		cursor = res.NextCursor
		p.mu.Lock()
		p.cursor = cursor
		p.mu.Unlock()
		if !res.HasMore {
			return
		}
	}
}

func (p *Platform) duplicate(id string) bool {
	if id == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for key, seenAt := range p.seen {
		if now.Sub(seenAt) > 10*time.Minute {
			delete(p.seen, key)
		}
	}
	if _, ok := p.seen[id]; ok {
		return true
	}
	p.seen[id] = now
	return false
}

func (p *Platform) accessToken(ctx context.Context) (string, error) {
	p.token.mu.Lock()
	defer p.token.mu.Unlock()
	if p.token.value != "" && time.Until(p.token.expiresAt) > time.Minute {
		return p.token.value, nil
	}
	u := p.apiBaseURL + "/cgi-bin/gettoken?corpid=" + url.QueryEscape(p.corpID) + "&corpsecret=" + url.QueryEscape(p.corpSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		ErrCode    int    `json:"errcode"`
		ErrMsg     string `json:"errmsg"`
		AccessToken string `json:"access_token"`
		ExpiresIn  int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.ErrCode != 0 || body.AccessToken == "" {
		return "", fmt.Errorf("wecom-kf: gettoken %d %s", body.ErrCode, body.ErrMsg)
	}
	p.token.value, p.token.expiresAt = body.AccessToken, time.Now().Add(time.Duration(body.ExpiresIn)*time.Second)
	return p.token.value, nil
}

func (p *Platform) Reply(ctx context.Context, replyCtx any, content string) error {
	rc, ok := replyCtx.(replyContext)
	if !ok {
		return fmt.Errorf("wecom-kf: invalid reply context %T", replyCtx)
	}
	accessToken, err := p.accessToken(ctx)
	if err != nil {
		return err
	}
	for _, chunk := range wecomkf.SplitTextByBytes(core.StripMarkdown(content), 2048) {
		if err := p.client.SendText(ctx, accessToken, wecomkf.SendTextRequest{
			ToUser: rc.toUser, OpenKfID: rc.openKfID, MsgID: rc.msgID,
			Text: wecomkf.MessageText{Content: chunk},
		}); err != nil {
			return err
		}
	}
	return nil
}

func (p *Platform) Send(ctx context.Context, replyCtx any, content string) error {
	return p.Reply(ctx, replyCtx, content)
}

var _ core.Platform = (*Platform)(nil)
