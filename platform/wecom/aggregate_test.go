package wecom

import (
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

type handlerRecorder struct {
	mu   sync.Mutex
	msgs []*core.Message
	ch   chan *core.Message
}

func newHandlerRecorder() *handlerRecorder {
	return &handlerRecorder{ch: make(chan *core.Message, 8)}
}

func (r *handlerRecorder) handle(_ core.Platform, msg *core.Message) {
	r.mu.Lock()
	r.msgs = append(r.msgs, msg)
	r.mu.Unlock()
	r.ch <- msg
}

func (r *handlerRecorder) wait(t *testing.T) *core.Message {
	t.Helper()
	select {
	case m := <-r.ch:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("handler not called")
		return nil
	}
}

func (r *handlerRecorder) expectNone(t *testing.T) {
	t.Helper()
	select {
	case m := <-r.ch:
		t.Fatalf("unexpected handler call: %+v", m)
	case <-time.After(80 * time.Millisecond):
	}
}

func newAggPlatform(window time.Duration) (*Platform, *handlerRecorder) {
	rec := newHandlerRecorder()
	p := &Platform{
		mergeWindow: window,
		agg:         make(map[string]*httpAggregate),
		handler:     rec.handle,
	}
	return p, rec
}

func TestHTTPAggregate_FileThenQuestionBecomesOneTurn(t *testing.T) {
	p, rec := newAggPlatform(50 * time.Millisecond)
	t.Cleanup(func() { p.dropAggregates() })

	sessionKey := "wecom:u1"
	fileBase := &core.Message{
		SessionKey: sessionKey, Platform: "wecom", MessageID: "f1",
		UserID: "u1", ReplyCtx: replyContext{userID: "u1"},
	}
	p.reserve(fileBase)

	p.emit(&core.Message{
		SessionKey: sessionKey, Platform: "wecom", MessageID: "t1",
		UserID: "u1", Content: "这个文件讲了什么？",
		ReplyCtx: replyContext{userID: "u1"},
	})

	time.Sleep(120 * time.Millisecond) // window elapsed, download still pending
	rec.expectNone(t)

	fileBase.Files = []core.FileAttachment{{
		FileName: "spec.pdf", MimeType: "application/pdf", Data: []byte("%PDF"),
	}}
	p.complete(sessionKey, fileBase)

	got := rec.wait(t)
	rec.expectNone(t)
	if got.Content != "这个文件讲了什么？" || len(got.Files) != 1 || got.Files[0].FileName != "spec.pdf" {
		t.Fatalf("merged = %+v files=%+v", got.Content, got.Files)
	}
}

func TestHTTPAggregate_TextOnlyMergesWithinWindow(t *testing.T) {
	p, rec := newAggPlatform(80 * time.Millisecond)
	t.Cleanup(func() { p.dropAggregates() })

	sessionKey := "wecom:alice"
	p.emit(&core.Message{SessionKey: sessionKey, MessageID: "a1", UserID: "alice", Content: "first"})
	p.emit(&core.Message{SessionKey: sessionKey, MessageID: "a2", UserID: "alice", Content: "second"})
	p.emit(&core.Message{SessionKey: "wecom:bob", MessageID: "b1", UserID: "bob", Content: "bob's question"})

	got := map[string]*core.Message{}
	for i := 0; i < 2; i++ {
		m := rec.wait(t)
		got[m.UserID] = m
	}
	rec.expectNone(t)
	if got["alice"].Content != "first\nsecond" || got["alice"].MessageID != "a2" {
		t.Fatalf("alice = %+v", got["alice"])
	}
	if got["bob"].Content != "bob's question" {
		t.Fatalf("bob = %+v", got["bob"])
	}
}

func TestHTTPAggregate_DisabledPassthrough(t *testing.T) {
	p, rec := newAggPlatform(0)
	p.emit(&core.Message{SessionKey: "wecom:u", MessageID: "1", Content: "hi"})
	got := rec.wait(t)
	if got.Content != "hi" {
		t.Fatalf("got %+v", got)
	}
}

func TestHTTPAggregate_FailedDownloadKeepsText(t *testing.T) {
	p, rec := newAggPlatform(40 * time.Millisecond)
	t.Cleanup(func() { p.dropAggregates() })

	sessionKey := "wecom:u"
	base := &core.Message{SessionKey: sessionKey, MessageID: "img", UserID: "u"}
	p.reserve(base)
	p.emit(&core.Message{SessionKey: sessionKey, MessageID: "t", UserID: "u", Content: "see picture"})
	time.Sleep(80 * time.Millisecond)
	p.complete(sessionKey, nil)

	got := rec.wait(t)
	if got.Content != "see picture" || len(got.Images) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestNew_DefaultMergeWindow(t *testing.T) {
	pf, err := New(map[string]any{
		"corp_id":          "ww_test",
		"corp_secret":      "sec_test",
		"agent_id":         "1000002",
		"callback_token":   "cb_token",
		"callback_aes_key": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	})
	if err != nil {
		t.Fatal(err)
	}
	p := pf.(*Platform)
	if p.mergeWindow != defaultMergeWindow {
		t.Fatalf("mergeWindow = %v, want %v", p.mergeWindow, defaultMergeWindow)
	}
}

func TestNew_MergeWindowMsZeroDisables(t *testing.T) {
	pf, err := New(map[string]any{
		"corp_id":          "ww_test",
		"corp_secret":      "sec_test",
		"agent_id":         "1000002",
		"callback_token":   "cb_token",
		"callback_aes_key": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"merge_window_ms":  int64(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if pf.(*Platform).mergeWindow != 0 {
		t.Fatalf("want disabled")
	}
}
