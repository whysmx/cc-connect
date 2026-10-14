package wecomkf

import (
	"strings"
	"testing"
	"time"
)

const welcomeText = "您好，我是项目助手，请直接提问。"

func enterSession(msgID, kfID, user, code string, sent time.Time) kfMessage {
	return kfMessage{MsgID: msgID, SendTime: sent.Unix(), Origin: originSystem, MsgType: "event",
		Event: &kfEvent{EventType: "enter_session", OpenKfID: kfID, ExternalUserID: user, Scene: "menu", WelcomeCode: code}}
}

func TestWelcome_SentOnEnterSessionViaCallback(t *testing.T) {
	f := newFakeWeCom(t)
	p := newTestPlatform(t, f, platformOpts{kfID: "wk1", path: "/kf-welcome", extra: map[string]any{"welcome_message": welcomeText}})
	rec := newRecorder()
	startPlatform(t, p, rec)

	ev := enterSession("e1", "wk1", "u1", "WELCOME-CODE", time.Now())
	f.addPage("wk1", syncMsgResponse{NextCursor: "c1", MsgList: []kfMessage{ev}})
	postNotify(t, p, "wk1", "t1")
	waitFor(t, "welcome message", func() bool { return len(f.welcomeMessages()) == 1 })
	if got := f.welcomeMessages()[0]; got.Code != "WELCOME-CODE" || got.Content != welcomeText {
		t.Fatalf("welcome = %+v", got)
	}
	rec.expectNone(t) // the event itself never reaches the agent

	// The same event pulled again (retry) is not welcomed twice.
	f.addPage("wk1", syncMsgResponse{NextCursor: "c2", MsgList: []kfMessage{ev}})
	postNotify(t, p, "wk1", "t2")
	waitFor(t, "second pull", func() bool { return p.store.Cursor() == "c2" })
	if n := len(f.welcomeMessages()); n != 1 {
		t.Fatalf("welcome sent %d times for one event", n)
	}
}

func TestWelcome_Skipped(t *testing.T) {
	now := time.Now()
	cases := map[string]struct {
		opts map[string]any
		ev   kfMessage
	}{
		"not configured":       {nil, enterSession("e", "wk1", "u", "code", now)},
		"no welcome_code":      {map[string]any{"welcome_message": welcomeText}, enterSession("e", "wk1", "u", "", now)},
		"code expired":         {map[string]any{"welcome_message": welcomeText}, enterSession("e", "wk1", "u", "code", now.Add(-time.Minute))},
		"other account":        {map[string]any{"welcome_message": welcomeText}, enterSession("e", "wk2", "u", "code", now)},
		"rejected by allow":    {map[string]any{"welcome_message": welcomeText, "allow_from": "vip"}, enterSession("e", "wk1", "u", "code", now)},
		"other event type":     {map[string]any{"welcome_message": welcomeText}, kfMessage{MsgID: "e", Origin: originSystem, MsgType: "event", Event: &kfEvent{EventType: "msg_send_fail", OpenKfID: "wk1", WelcomeCode: "code"}}},
		"event without detail": {map[string]any{"welcome_message": welcomeText}, kfMessage{MsgID: "e", Origin: originSystem, MsgType: "event"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeWeCom(t)
			p, rec := newDirectPlatform(t, f, tc.opts)
			p.processMessage(p.ctx, &tc.ev)
			rec.expectNone(t)
			if n := len(f.welcomeMessages()); n != 0 {
				t.Fatalf("welcome sent %d times", n)
			}
		})
	}
}

func TestWelcome_RefreshesTokenAndSurvivesFailures(t *testing.T) {
	orig := retryDelay
	retryDelay = func(int) time.Duration { return time.Millisecond }
	defer func() { retryDelay = orig }()

	f := newFakeWeCom(t)
	p, _ := newDirectPlatform(t, f, map[string]any{"welcome_message": welcomeText})

	// A permanent failure (e.g. 95001 code already used) is logged, not retried.
	f.setFail("/cgi-bin/kf/send_msg_on_event", 95001)
	ev := enterSession("e1", "wk1", "u", "code", time.Now())
	p.processMessage(p.ctx, &ev)

	// An expired access_token is refreshed and the welcome still goes out.
	f.mu.Lock()
	delete(f.fail, "/cgi-bin/kf/send_msg_on_event")
	f.expireNext = true // first attempt: stale token, refreshed transparently
	f.mu.Unlock()
	ev2 := enterSession("e2", "wk1", "u2", "code2", time.Now())
	p.processMessage(p.ctx, &ev2)
	if got := f.welcomeMessages(); len(got) != 1 || got[0].Code != "code2" {
		t.Fatalf("welcomes = %+v", got)
	}
}

func TestNew_WelcomeMessageTruncatedToOneMessage(t *testing.T) {
	opts := map[string]any{"corp_id": "c", "corp_secret": "s", "open_kfid": "wk", "callback_token": "t",
		"callback_aes_key": testAESKeyEncoded, "welcome_message": strings.Repeat("欢", 1000)}
	p, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(p.(*Platform).welcomeMessage); n > maxTextBytes || n == 0 {
		t.Fatalf("welcome message is %d bytes", n)
	}
}
