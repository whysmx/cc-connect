package wecomkf

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNew_OptionParsing(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{"corp_id": "c", "corp_secret": "s", "open_kfid": "wk", "callback_token": "t", "callback_aes_key": testAESKeyEncoded}
	}
	with := func(k string, v any) map[string]any {
		o := base()
		o[k] = v
		return o
	}
	mustNew := func(o map[string]any) *Platform {
		t.Helper()
		p, err := New(o)
		if err != nil {
			t.Fatalf("New(%v): %v", o, err)
		}
		return p.(*Platform)
	}

	if p := mustNew(with("callback_path", "kf/cb")); p.callbackPath != "/kf/cb" {
		t.Fatalf("callback_path = %q, want leading slash added", p.callbackPath)
	}
	if p := mustNew(with("takeover_check", false)); p.takeoverCheck {
		t.Fatal("takeover_check=false ignored")
	}
	for _, v := range []any{int64(2), 2, float64(2)} {
		if p := mustNew(with("max_replies_per_message", v)); p.maxReplies != 2 {
			t.Fatalf("max_replies_per_message %T = %d, want 2", v, p.maxReplies)
		}
	}
	if p := mustNew(with("max_replies_per_message", int64(-1))); p.maxReplies != defaultMaxReplies {
		t.Fatalf("negative max_replies_per_message should fall back to default, got %d", p.maxReplies)
	}
	if p := mustNew(with("proxy", "http://127.0.0.1:8888")); p.api.http.Transport == nil {
		t.Fatal("proxy transport missing")
	}
	if _, err := New(with("proxy", "http://[::1")); err == nil {
		t.Fatal("invalid proxy accepted")
	}
	if _, err := New(with("callback_aes_key", "short")); err == nil {
		t.Fatal("invalid callback_aes_key accepted")
	}
}

func TestFormattingInstructionsAndSend(t *testing.T) {
	f := newFakeWeCom(t)
	p := newTestPlatform(t, f, platformOpts{kfID: "wk1"})
	fi := p.FormattingInstructions()
	for _, want := range []string{"plain text", "Markdown", "read-only"} {
		if !strings.Contains(fi, want) {
			t.Errorf("FormattingInstructions missing %q", want)
		}
	}
	if err := p.Send(context.Background(), replyContext{openKfID: "wk1", externalUserID: "u"}, "proactive"); err != nil {
		t.Fatal(err)
	}
	if sent := f.sentMessages(); len(sent) != 1 || sent[0].Content != "proactive" {
		t.Fatalf("Send = %+v", sent)
	}
	if err := p.Reply(context.Background(), "not a reply ctx", "x"); err == nil {
		t.Fatal("invalid reply context accepted")
	}
	if err := p.Reply(context.Background(), replyContext{openKfID: "wk1", externalUserID: "u"}, "  \n\t "); err != nil {
		t.Fatal(err)
	}
	if len(f.sentMessages()) != 1 {
		t.Fatal("whitespace-only reply must not be sent")
	}
}

func TestReply_ReturnsSendErrors(t *testing.T) {
	f := newFakeWeCom(t)
	p := newTestPlatform(t, f, platformOpts{kfID: "wk1"})
	f.setFail("/cgi-bin/kf/send_msg", 95018)
	err := p.Reply(context.Background(), replyContext{openKfID: "wk1", externalUserID: "u"}, "hello")
	if err == nil || !strings.Contains(err.Error(), "95018") {
		t.Fatalf("err = %v", err)
	}
}

func TestTakeoverCheck_FailsOpenWhenStateUnavailable(t *testing.T) {
	f := newFakeWeCom(t)
	p := newTestPlatform(t, f, platformOpts{kfID: "wk1"})
	f.setFail("/cgi-bin/kf/service_state/get", 48002) // API not authorized
	if !p.aiAllowed(context.Background(), "u", "reply") {
		t.Fatal("state lookup failure must fail open")
	}
}

// newDirectPlatform returns a platform ready for processMessage/syncOnce
// calls without starting the shared listener.
func newDirectPlatform(t *testing.T, f *fakeWeCom, extra map[string]any) (*Platform, *recorder) {
	t.Helper()
	p := newTestPlatform(t, f, platformOpts{kfID: "wk1", extra: extra})
	rec := newRecorder()
	p.handler = rec.handle
	p.ctx, p.cancel = context.WithCancel(context.Background())
	t.Cleanup(p.cancel)
	return p, rec
}

func TestProcessMessage_Filters(t *testing.T) {
	f := newFakeWeCom(t)
	p, rec := newDirectPlatform(t, f, map[string]any{"allow_from": "vip"})
	now := time.Now().Unix()

	drop := []kfMessage{
		{MsgID: "o1", OpenKfID: "wk-other", ExternalUserID: "vip", SendTime: now, Origin: originCustomer, MsgType: "text", Text: &kfText{Content: "x"}},
		{MsgID: "o2", OpenKfID: "wk1", SendTime: now, Origin: originCustomer, MsgType: "text", Text: &kfText{Content: "no user"}},
		{MsgID: "o3", OpenKfID: "wk1", ExternalUserID: "stranger", SendTime: now, Origin: originCustomer, MsgType: "text", Text: &kfText{Content: "x"}},
		{MsgID: "o4", OpenKfID: "wk1", ExternalUserID: "vip", SendTime: now, Origin: originCustomer, MsgType: "text", Text: &kfText{Content: "   "}},
		{MsgID: "o5", OpenKfID: "wk1", ExternalUserID: "vip", SendTime: now, Origin: 99, MsgType: "text", Text: &kfText{Content: "x"}},
		{MsgID: "o6", Origin: originSystem, MsgType: "event", Event: &kfEvent{EventType: "msg_send_fail", OpenKfID: "wk1", ExternalUserID: "vip", FailMsgID: "m", FailType: 6}},
		{MsgID: "o7", Origin: originSystem, MsgType: "event", Event: &kfEvent{EventType: "servicer_status_change", OpenKfID: "wk1"}},
		{MsgID: "o8", Origin: originSystem, MsgType: "event"},
	}
	for i := range drop {
		p.processMessage(p.ctx, &drop[i])
	}
	rec.expectNone(t)

	ok := customerText("ok1", "wk1", "VIP", "allowed (case-insensitive allow_from)")
	p.processMessage(p.ctx, &ok)
	if got := rec.wait(t); got.MessageID != "ok1" {
		t.Fatalf("dispatched %s", got.MessageID)
	}
}

func TestSyncOnce_PaginationErrorsAndCancellation(t *testing.T) {
	t.Run("follows has_more across pages", func(t *testing.T) {
		f := newFakeWeCom(t)
		p, rec := newDirectPlatform(t, f, nil)
		f.addPage("wk1", syncMsgResponse{NextCursor: "p1", HasMore: 1, MsgList: []kfMessage{customerText("a", "wk1", "u1", "1")}})
		f.addPage("wk1", syncMsgResponse{NextCursor: "p2", HasMore: 1}) // empty page with has_more=1
		f.addPage("wk1", syncMsgResponse{NextCursor: "p3", MsgList: []kfMessage{customerText("b", "wk1", "u2", "2")}})
		p.syncOnce(p.ctx, "tok")
		rec.wait(t)
		rec.wait(t)
		reqs := f.syncRequests()
		if len(reqs) != 3 || reqs[1].Cursor != "p1" || reqs[2].Cursor != "p2" || p.store.Cursor() != "p3" {
			t.Fatalf("requests=%+v cursor=%q", reqs, p.store.Cursor())
		}
	})
	t.Run("api error keeps cursor", func(t *testing.T) {
		f := newFakeWeCom(t)
		p, _ := newDirectPlatform(t, f, nil)
		p.store.SetCursor("keep")
		f.setFail("/cgi-bin/kf/sync_msg", 95007)
		p.syncOnce(p.ctx, "expired-token")
		if p.store.Cursor() != "keep" {
			t.Fatalf("cursor changed to %q after failure", p.store.Cursor())
		}
	})
	t.Run("page limit bounds one pull", func(t *testing.T) {
		f := newFakeWeCom(t)
		p, _ := newDirectPlatform(t, f, nil)
		for i := 0; i < maxSyncPages+5; i++ {
			f.addPage("wk1", syncMsgResponse{NextCursor: "c", HasMore: 1})
		}
		p.syncOnce(p.ctx, "")
		if n := len(f.syncRequests()); n != maxSyncPages {
			t.Fatalf("sync calls = %d, want %d", n, maxSyncPages)
		}
	})
	t.Run("cancelled context stops", func(t *testing.T) {
		f := newFakeWeCom(t)
		p, _ := newDirectPlatform(t, f, nil)
		p.cancel()
		p.syncOnce(p.ctx, "")
		if n := len(f.syncRequests()); n != 0 {
			t.Fatalf("sync calls after cancel = %d", n)
		}
	})
	t.Run("persist failure does not stop processing", func(t *testing.T) {
		f := newFakeWeCom(t)
		p, rec := newDirectPlatform(t, f, nil)
		p.store.path = t.TempDir() // a directory: rename onto it fails
		f.addPage("wk1", syncMsgResponse{NextCursor: "c1", MsgList: []kfMessage{customerText("m", "wk1", "u", "q")}})
		p.syncOnce(p.ctx, "")
		rec.wait(t)
		if p.store.Cursor() != "c1" {
			t.Fatalf("cursor = %q", p.store.Cursor())
		}
	})
}

func TestTriggerSync_CoalescesConcurrentNotifications(t *testing.T) {
	f := newFakeWeCom(t)
	p, _ := newDirectPlatform(t, f, nil)

	var mu sync.Mutex
	var tokens []string
	entered := make(chan struct{})
	release := make(chan struct{})
	srv := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cgi-bin/gettoken" {
			_, _ = w.Write([]byte(`{"errcode":0,"access_token":"t","expires_in":7200}`))
			return
		}
		var req syncMsgRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		tokens = append(tokens, req.Token)
		first := len(tokens) == 1
		mu.Unlock()
		if first {
			close(entered)
			<-release
		}
		_, _ = w.Write([]byte(`{"errcode":0,"next_cursor":"c"}`))
	})
	p.api = newAPIClient(srv.URL, "c", "s", srv.Client())

	p.triggerSync("t1")
	<-entered // first pull is in flight
	for i := 0; i < 5; i++ {
		p.triggerSync(fmt.Sprintf("t%d", i+2))
	}
	close(release)
	p.wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(tokens) != 2 || tokens[0] != "t1" || tokens[1] != "t6" {
		t.Fatalf("pull tokens = %v, want [t1 t6] (one coalesced follow-up with the newest token)", tokens)
	}
}

func TestSendWithRetry_StopsOnContextCancel(t *testing.T) {
	f := newFakeWeCom(t)
	p := newTestPlatform(t, f, platformOpts{kfID: "wk1"})
	f.setFail("/cgi-bin/kf/send_msg", -1)
	orig := retryDelay
	retryDelay = func(int) time.Duration { return time.Hour }
	defer func() { retryDelay = orig }()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if err := p.sendWithRetry(ctx, replyContext{openKfID: "wk1", externalUserID: "u"}, "x"); err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestTakeBudget_DefaultsForUnknownCustomer(t *testing.T) {
	f := newFakeWeCom(t)
	p := newTestPlatform(t, f, platformOpts{kfID: "wk1"})
	if got := p.takeBudget("new-user", 2); got != 2 {
		t.Fatalf("granted %d, want 2", got)
	}
	if got := p.takeBudget("new-user", 10); got != defaultMaxReplies-2 {
		t.Fatalf("granted %d, want %d", got, defaultMaxReplies-2)
	}
}
