package wecomkf

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// fakeWeCom is a minimal WeChat Customer Service API server.
type fakeWeCom struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	pages      map[string][]syncMsgResponse // open_kfid -> pages served in order
	syncCalls  []syncMsgRequest
	states     map[string]int // external_userid -> service_state
	sent       []sentMsg
	tokenCalls int
	expireNext bool // next API call answers 42001
}

type sentMsg struct {
	OpenKfID, ToUser, Content string
}

func newFakeWeCom(t *testing.T) *fakeWeCom {
	f := &fakeWeCom{t: t, pages: map[string][]syncMsgResponse{}, states: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeWeCom) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/cgi-bin/gettoken" {
		f.tokenCalls++
		_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 0, "access_token": fmt.Sprintf("tok%d", f.tokenCalls), "expires_in": 7200})
		return
	}
	if f.expireNext {
		f.expireNext = false
		_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 42001, "errmsg": "access_token expired"})
		return
	}
	body, _ := io.ReadAll(r.Body)
	switch r.URL.Path {
	case "/cgi-bin/kf/sync_msg":
		var req syncMsgRequest
		_ = json.Unmarshal(body, &req)
		f.syncCalls = append(f.syncCalls, req)
		resp := syncMsgResponse{NextCursor: req.Cursor}
		if pages := f.pages[req.OpenKfID]; len(pages) > 0 {
			resp = pages[0]
			f.pages[req.OpenKfID] = pages[1:]
		}
		_ = json.NewEncoder(w).Encode(resp)
	case "/cgi-bin/kf/service_state/get":
		var req map[string]string
		_ = json.Unmarshal(body, &req)
		_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 0, "service_state": f.states[req["external_userid"]]})
	case "/cgi-bin/kf/send_msg":
		var req struct {
			ToUser   string `json:"touser"`
			OpenKfID string `json:"open_kfid"`
			Text     struct {
				Content string `json:"content"`
			} `json:"text"`
		}
		_ = json.Unmarshal(body, &req)
		f.sent = append(f.sent, sentMsg{OpenKfID: req.OpenKfID, ToUser: req.ToUser, Content: req.Text.Content})
		_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 0, "msgid": "sent"})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeWeCom) addPage(openKfID string, page syncMsgResponse) {
	f.mu.Lock()
	f.pages[openKfID] = append(f.pages[openKfID], page)
	f.mu.Unlock()
}

func (f *fakeWeCom) setState(user string, state int) {
	f.mu.Lock()
	f.states[user] = state
	f.mu.Unlock()
}

func (f *fakeWeCom) sentMessages() []sentMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentMsg(nil), f.sent...)
}

func (f *fakeWeCom) syncRequests() []syncMsgRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]syncMsgRequest(nil), f.syncCalls...)
}

func customerText(msgID, kfID, user, text string) kfMessage {
	return kfMessage{MsgID: msgID, OpenKfID: kfID, ExternalUserID: user, SendTime: time.Now().Unix(),
		Origin: originCustomer, MsgType: "text", Text: &kfText{Content: text}}
}

// recorder collects messages handed to the engine.
type recorder struct {
	mu   sync.Mutex
	msgs []*core.Message
	ch   chan *core.Message
}

func newRecorder() *recorder { return &recorder{ch: make(chan *core.Message, 32)} }

func (r *recorder) handle(_ core.Platform, m *core.Message) {
	r.mu.Lock()
	r.msgs = append(r.msgs, m)
	r.mu.Unlock()
	r.ch <- m
}

func (r *recorder) wait(t *testing.T) *core.Message {
	t.Helper()
	select {
	case m := <-r.ch:
		return m
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for dispatched message")
		return nil
	}
}

func (r *recorder) expectNone(t *testing.T) {
	t.Helper()
	select {
	case m := <-r.ch:
		t.Fatalf("unexpected dispatched message: %+v", m)
	case <-time.After(150 * time.Millisecond):
	}
}

type platformOpts struct {
	kfID, project, listen, path, dataDir, token string
	extra                                       map[string]any
}

func newTestPlatform(t *testing.T, f *fakeWeCom, o platformOpts) *Platform {
	t.Helper()
	if o.listen == "" {
		o.listen = "127.0.0.1:0"
	}
	if o.token == "" {
		o.token = "cbtoken"
	}
	opts := map[string]any{
		"corp_id": "corp1", "corp_secret": "secret", "open_kfid": o.kfID,
		"callback_token": o.token, "callback_aes_key": testAESKeyEncoded,
		"listen_addr": o.listen, "callback_path": o.path, "api_base_url": f.srv.URL,
		"cc_project": o.project, "cc_data_dir": o.dataDir,
	}
	for k, v := range o.extra {
		opts[k] = v
	}
	p, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p.(*Platform)
}

func startPlatform(t *testing.T, p *Platform, rec *recorder) {
	t.Helper()
	if err := p.Start(rec.handle); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })
}

// postNotify sends an encrypted kf_msg_or_event callback to the shared hub.
func postNotify(t *testing.T, p *Platform, openKfID, pullToken string) {
	t.Helper()
	h := lookupHub(p.listenAddr)
	if h == nil {
		t.Fatal("hub not running")
	}
	inner := fmt.Sprintf("<xml><ToUserName>corp1</ToUserName><CreateTime>1</CreateTime><MsgType>event</MsgType><Event>kf_msg_or_event</Event><Token>%s</Token><OpenKfId>%s</OpenKfId></xml>", pullToken, openKfID)
	enc := encryptForTest(t, p.aesKey, inner, "corp1")
	sig := computeSignature(p.callbackToken, "1700000000", "n1", enc)
	u := fmt.Sprintf("http://%s%s?msg_signature=%s&timestamp=1700000000&nonce=n1", h.listener.Addr(), p.callbackPath, sig)
	resp, err := http.Post(u, "text/xml", strings.NewReader("<xml><ToUserName>corp1</ToUserName><Encrypt>"+enc+"</Encrypt></xml>"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "success" {
		t.Fatalf("callback status=%d body=%q", resp.StatusCode, body)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestNew_ValidatesRequiredOptions(t *testing.T) {
	base := map[string]any{"corp_id": "c", "corp_secret": "s", "open_kfid": "wk", "callback_token": "t", "callback_aes_key": testAESKeyEncoded}
	for _, missing := range []string{"corp_id", "corp_secret", "open_kfid", "callback_token", "callback_aes_key"} {
		opts := map[string]any{}
		for k, v := range base {
			if k != missing {
				opts[k] = v
			}
		}
		if _, err := New(opts); err == nil {
			t.Fatalf("New without %s succeeded", missing)
		}
	}
	p, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	kp := p.(*Platform)
	if kp.Name() != "wecom_kf" || kp.listenAddr != defaultListenAddr || kp.callbackPath != defaultCallbackPath || !kp.takeoverCheck || kp.maxReplies != 5 {
		t.Fatalf("unexpected defaults: %+v", kp)
	}
	bad := map[string]any{}
	for k, v := range base {
		bad[k] = v
	}
	bad["api_base_url"] = "ftp://x"
	if _, err := New(bad); err == nil {
		t.Fatal("invalid api_base_url accepted")
	}
}

func TestCallbackURLVerification(t *testing.T) {
	f := newFakeWeCom(t)
	p := newTestPlatform(t, f, platformOpts{kfID: "wk1", path: "/kf-verify"})
	startPlatform(t, p, newRecorder())

	echo := encryptForTest(t, p.aesKey, "echo-123", "corp1")
	sig := computeSignature(p.callbackToken, "1", "n", echo)
	base := fmt.Sprintf("http://%s/kf-verify?timestamp=1&nonce=n&echostr=%s", lookupHub(p.listenAddr).listener.Addr(), url.QueryEscape(echo))

	resp, err := http.Get(base + "&msg_signature=" + sig)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "echo-123" {
		t.Fatalf("verify status=%d body=%q", resp.StatusCode, body)
	}

	resp, err = http.Get(base + "&msg_signature=deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bad signature status = %d, want 403", resp.StatusCode)
	}
}

func TestEndToEnd_PullDispatchAndReply(t *testing.T) {
	f := newFakeWeCom(t)
	p := newTestPlatform(t, f, platformOpts{kfID: "wk1", project: "proj-a", path: "/kf-e2e", dataDir: t.TempDir()})
	rec := newRecorder()
	startPlatform(t, p, rec)

	f.addPage("wk1", syncMsgResponse{NextCursor: "c1", MsgList: []kfMessage{
		customerText("m1", "wk1", "userA", "什么是 cc-connect？"),
		{MsgID: "m2", OpenKfID: "wk1", ExternalUserID: "userA", Origin: originServicer, ServicerUserID: "agent1", MsgType: "text", Text: &kfText{Content: "human reply"}, SendTime: time.Now().Unix()},
		{MsgID: "m3", Origin: originSystem, MsgType: "event", Event: &kfEvent{EventType: "session_status_change", OpenKfID: "wk1", ExternalUserID: "userA", ChangeType: 1}},
		{MsgID: "m4", OpenKfID: "wk1", ExternalUserID: "userA", Origin: originCustomer, MsgType: "image", SendTime: time.Now().Unix()},
	}})
	postNotify(t, p, "wk1", "pull-token")

	msg := rec.wait(t)
	rec.expectNone(t) // servicer message, event and image are not dispatched
	if msg.SessionKey != "wecom_kf:corp1:wk1:userA" || msg.Platform != "wecom_kf" || msg.UserID != "userA" || msg.Content != "什么是 cc-connect？" || msg.MessageID != "m1" {
		t.Fatalf("unexpected message: %+v", msg)
	}
	reqs := f.syncRequests()
	if len(reqs) == 0 || reqs[0].Token != "pull-token" || reqs[0].OpenKfID != "wk1" || reqs[0].Cursor != "" {
		t.Fatalf("unexpected sync requests: %+v", reqs)
	}

	if err := p.Reply(context.Background(), msg.ReplyCtx, "**答案**：一个桥接工具"); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	sent := f.sentMessages()
	if len(sent) != 1 || sent[0].ToUser != "userA" || sent[0].OpenKfID != "wk1" || strings.Contains(sent[0].Content, "**") {
		t.Fatalf("unexpected sent messages: %+v", sent)
	}
	waitFor(t, "cursor persisted", func() bool { return p.store.Cursor() == "c1" })
}

func TestMultiAccount_SharedListenerRoutesByOpenKfID(t *testing.T) {
	f := newFakeWeCom(t)
	listen := "127.0.0.1:0"
	pa := newTestPlatform(t, f, platformOpts{kfID: "wkA", project: "proj-a", listen: listen, path: "/kf-multi"})
	pb := newTestPlatform(t, f, platformOpts{kfID: "wkB", project: "proj-b", listen: listen, path: "/kf-multi"})
	recA, recB := newRecorder(), newRecorder()
	startPlatform(t, pa, recA)
	startPlatform(t, pb, recB)

	if lookupHub(listen).route("/kf-multi") == nil || len(lookupHub(listen).route("/kf-multi").accounts) != 2 {
		t.Fatal("both accounts should share one route on one listener")
	}

	// The same customer talks to both accounts; another customer to A.
	f.addPage("wkA", syncMsgResponse{NextCursor: "a1", MsgList: []kfMessage{
		customerText("a-1", "wkA", "user1", "q to A"),
		customerText("a-2", "wkA", "user2", "q2 to A"),
	}})
	f.addPage("wkB", syncMsgResponse{NextCursor: "b1", MsgList: []kfMessage{customerText("b-1", "wkB", "user1", "q to B")}})

	postNotify(t, pa, "wkA", "tA")
	postNotify(t, pb, "wkB", "tB")

	gotA := map[string]bool{recA.wait(t).SessionKey: true, recA.wait(t).SessionKey: true}
	gotB := recB.wait(t)
	if !gotA["wecom_kf:corp1:wkA:user1"] || !gotA["wecom_kf:corp1:wkA:user2"] {
		t.Fatalf("account A sessions = %v", gotA)
	}
	if gotB.SessionKey != "wecom_kf:corp1:wkB:user1" {
		t.Fatalf("account B session = %s", gotB.SessionKey)
	}

	// Removing one account keeps the shared listener alive for the other.
	if err := pa.Stop(); err != nil {
		t.Fatal(err)
	}
	if h := lookupHub(listen); h == nil || h.account("/kf-multi", "wkB") == nil || h.account("/kf-multi", "wkA") != nil {
		t.Fatal("stopping one account must not affect the other")
	}
}

func TestMultiAccount_RejectsDuplicateAndConflictingMappings(t *testing.T) {
	f := newFakeWeCom(t)
	listen := "127.0.0.1:0"
	p1 := newTestPlatform(t, f, platformOpts{kfID: "wkDup", project: "p1", listen: listen, path: "/kf-dup"})
	startPlatform(t, p1, newRecorder())

	p2 := newTestPlatform(t, f, platformOpts{kfID: "wkDup", project: "p2", listen: listen, path: "/kf-dup"})
	if err := p2.Start(newRecorder().handle); err == nil || !strings.Contains(err.Error(), "exactly one project") {
		t.Fatalf("duplicate open_kfid err = %v", err)
	}
	p3 := newTestPlatform(t, f, platformOpts{kfID: "wkOther", project: "p3", listen: listen, path: "/kf-dup", token: "different"})
	if err := p3.Start(newRecorder().handle); err == nil || !strings.Contains(err.Error(), "different") {
		t.Fatalf("conflicting credentials err = %v", err)
	}
	if lookupHub(listen).account("/kf-dup", "wkDup") != p1 {
		t.Fatal("original mapping must stay intact")
	}
}

func TestDedup_RepeatedNotificationsAndRestartDoNotReplay(t *testing.T) {
	f := newFakeWeCom(t)
	dataDir := t.TempDir()
	p := newTestPlatform(t, f, platformOpts{kfID: "wk1", path: "/kf-dedup", dataDir: dataDir})
	rec := newRecorder()
	startPlatform(t, p, rec)

	msg := customerText("dup-1", "wk1", "userA", "hello")
	// WeCom may return the same message again (retry / overlapping pulls).
	f.addPage("wk1", syncMsgResponse{NextCursor: "c1", HasMore: 1, MsgList: []kfMessage{msg}})
	f.addPage("wk1", syncMsgResponse{NextCursor: "c2", MsgList: []kfMessage{msg}})
	postNotify(t, p, "wk1", "t1")
	rec.wait(t)
	postNotify(t, p, "wk1", "t2")
	rec.expectNone(t)
	waitFor(t, "cursor c2", func() bool { return p.store.Cursor() == "c2" })
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}

	// Restart: the persisted cursor and seen IDs survive.
	p2 := newTestPlatform(t, f, platformOpts{kfID: "wk1", path: "/kf-dedup", dataDir: dataDir})
	if p2.skipBacklog {
		t.Fatal("restart with a persisted cursor must not skip the backlog")
	}
	rec2 := newRecorder()
	startPlatform(t, p2, rec2)
	f.addPage("wk1", syncMsgResponse{NextCursor: "c3", MsgList: []kfMessage{msg}})
	postNotify(t, p2, "wk1", "t3")
	rec2.expectNone(t)
	waitFor(t, "sync after restart", func() bool {
		reqs := f.syncRequests()
		return len(reqs) > 0 && reqs[len(reqs)-1].Cursor == "c2"
	})
}

func TestFirstStart_SkipsHistoryBeforeStartup(t *testing.T) {
	f := newFakeWeCom(t)
	p := newTestPlatform(t, f, platformOpts{kfID: "wk1", path: "/kf-history"})
	rec := newRecorder()
	startPlatform(t, p, rec)

	old := customerText("old-1", "wk1", "userA", "from yesterday")
	old.SendTime = time.Now().Add(-24 * time.Hour).Unix()
	f.addPage("wk1", syncMsgResponse{NextCursor: "c1", MsgList: []kfMessage{old, customerText("new-1", "wk1", "userA", "now")}})
	postNotify(t, p, "wk1", "t")
	if got := rec.wait(t); got.MessageID != "new-1" {
		t.Fatalf("dispatched %s, want new-1", got.MessageID)
	}
	rec.expectNone(t)
}

func TestHumanTakeover_SuppressesDispatchAndLateReplies(t *testing.T) {
	f := newFakeWeCom(t)
	p := newTestPlatform(t, f, platformOpts{kfID: "wk1", path: "/kf-takeover"})
	rec := newRecorder()
	startPlatform(t, p, rec)

	// A human servicer is already handling userH: nothing goes to the agent.
	f.setState("userH", stateHuman)
	f.setState("userQ", stateQueued)
	f.addPage("wk1", syncMsgResponse{NextCursor: "c1", MsgList: []kfMessage{
		customerText("h-1", "wk1", "userH", "need a human"),
		customerText("q-1", "wk1", "userQ", "queued"),
	}})
	postNotify(t, p, "wk1", "t1")
	rec.expectNone(t)

	// AI accepted the question, then a human took over while it was thinking.
	f.setState("userA", stateAssistant)
	f.addPage("wk1", syncMsgResponse{NextCursor: "c2", MsgList: []kfMessage{customerText("a-1", "wk1", "userA", "q")}})
	postNotify(t, p, "wk1", "t2")
	msg := rec.wait(t)
	f.setState("userA", stateHuman)
	if err := p.Reply(context.Background(), msg.ReplyCtx, "late AI answer"); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if sent := f.sentMessages(); len(sent) != 0 {
		t.Fatalf("AI answered during human takeover: %+v", sent)
	}

	// takeover_check = false disables the state lookup entirely.
	p.takeoverCheck = false
	if err := p.Reply(context.Background(), msg.ReplyCtx, "forced"); err != nil {
		t.Fatal(err)
	}
	if sent := f.sentMessages(); len(sent) != 1 {
		t.Fatalf("expected 1 message with takeover_check disabled, got %d", len(sent))
	}
}

func TestReply_SplitsAndRespectsPerMessageBudget(t *testing.T) {
	f := newFakeWeCom(t)
	p := newTestPlatform(t, f, platformOpts{kfID: "wk1", path: "/kf-budget", extra: map[string]any{"max_replies_per_message": int64(3)}})
	rc := replyContext{openKfID: "wk1", externalUserID: "userA"}
	p.budget["userA"] = p.maxReplies

	long := strings.Repeat("长", 5000) // 15000 bytes -> 8 chunks
	if err := p.Reply(context.Background(), rc, long); err != nil {
		t.Fatal(err)
	}
	sent := f.sentMessages()
	if len(sent) != 3 {
		t.Fatalf("sent %d chunks, want 3 (budget)", len(sent))
	}
	for i, s := range sent {
		if len(s.Content) > 2048 {
			t.Fatalf("chunk %d is %d bytes, exceeds 2048", i, len(s.Content))
		}
	}
	if !strings.HasSuffix(sent[2].Content, truncationMarker) {
		t.Fatal("last chunk should carry the truncation marker")
	}
	if err := p.Reply(context.Background(), rc, "more"); err != nil {
		t.Fatal(err)
	}
	if len(f.sentMessages()) != 3 {
		t.Fatal("budget exhausted: further replies must be dropped")
	}
}

func TestAPI_RefreshesExpiredAccessToken(t *testing.T) {
	f := newFakeWeCom(t)
	p := newTestPlatform(t, f, platformOpts{kfID: "wk1", path: "/kf-token"})
	if _, err := p.api.accessToken(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.expireNext = true
	f.mu.Unlock()
	if _, err := p.api.sendText(context.Background(), "wk1", "u", "hi"); err != nil {
		t.Fatalf("sendText after token expiry: %v", err)
	}
	f.mu.Lock()
	calls := f.tokenCalls
	f.mu.Unlock()
	if calls != 2 {
		t.Fatalf("gettoken calls = %d, want 2", calls)
	}
}

func TestSendWithRetry_RetriesTransientErrors(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cgi-bin/gettoken" {
			_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 0, "access_token": "t", "expires_in": 7200})
			return
		}
		if attempts.Add(1) < 3 {
			_ = json.NewEncoder(w).Encode(map[string]any{"errcode": -1, "errmsg": "system busy"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 0})
	}))
	defer srv.Close()
	orig := retryDelay
	retryDelay = func(int) time.Duration { return time.Millisecond }
	defer func() { retryDelay = orig }()

	p := &Platform{api: newAPIClient(srv.URL, "c", "s", srv.Client())}
	if err := p.sendWithRetry(context.Background(), replyContext{openKfID: "wk", externalUserID: "u"}, "x"); err != nil {
		t.Fatalf("sendWithRetry: %v", err)
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d, want 3", attempts.Load())
	}

	// A permanent error (e.g. 95018 session closed) is not retried.
	attempts.Store(0)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cgi-bin/gettoken" {
			_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 0, "access_token": "t", "expires_in": 7200})
			return
		}
		attempts.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 95018, "errmsg": "closed"})
	})
	if err := p.sendWithRetry(context.Background(), replyContext{openKfID: "wk", externalUserID: "u"}, "x"); err == nil {
		t.Fatal("expected permanent error")
	}
	if attempts.Load() != 1 {
		t.Fatalf("permanent error attempts = %d, want 1", attempts.Load())
	}
}

func TestReconstructReplyCtxAndSessionKey(t *testing.T) {
	f := newFakeWeCom(t)
	p := newTestPlatform(t, f, platformOpts{kfID: "wk1"})
	key := SessionKey("corp1", "wk1", "userA")
	rc, err := p.ReconstructReplyCtx(key)
	if err != nil {
		t.Fatal(err)
	}
	if got := rc.(replyContext); got.openKfID != "wk1" || got.externalUserID != "userA" {
		t.Fatalf("reply ctx = %+v", got)
	}
	for _, bad := range []string{"wecom:corp1:wk1:userA", "wecom_kf:corp1:wk2:userA", "wecom_kf:corp1:wk1", "wecom_kf:corp1:wk1:"} {
		if _, err := p.ReconstructReplyCtx(bad); err == nil {
			t.Fatalf("ReconstructReplyCtx(%q) accepted", bad)
		}
	}
}

func TestSplitText(t *testing.T) {
	if got := splitText("你好世界", 7); len(got) != 2 || got[0] != "你好" || got[1] != "世界" {
		t.Fatalf("rune-safe split = %#v", got)
	}
	if got := splitText("line1-abc\nline2-long", 14); got[0] != "line1-abc\n" {
		t.Fatalf("newline-preferred split = %#v", got)
	}
	if got := splitText("", 10); len(got) != 0 {
		t.Fatalf("empty = %#v", got)
	}
}
