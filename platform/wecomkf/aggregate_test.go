package wecomkf

import (
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func newMergingPlatform(t *testing.T, f *fakeWeCom, windowMs int64) (*Platform, *recorder) {
	t.Helper()
	p, rec := newDirectPlatform(t, f, map[string]any{"merge_window_ms": windowMs})
	t.Cleanup(func() { _ = p.Stop() })
	return p, rec
}

func TestMerge_FileThenQuestionBecomesOneTurn(t *testing.T) {
	f := newFakeWeCom(t)
	f.addMedia("doc", fakeMedia{data: []byte("%PDF-1.4"), disposition: `attachment; filename="spec.pdf"`})
	gate := make(chan struct{})
	f.mediaGate = gate // the download outlives the merge window
	p, rec := newMergingPlatform(t, f, 50)

	file := mediaMsg("f1", "file", "doc", "u1")
	question := customerText("t1", "wk1", "u1", "这个文件讲了什么？")
	p.processMessage(p.ctx, &file)
	p.processMessage(p.ctx, &question)

	time.Sleep(150 * time.Millisecond) // window elapsed, download still pending
	rec.expectNone(t)
	close(gate)

	got := rec.wait(t)
	rec.expectNone(t)
	if got.Content != "这个文件讲了什么？" || len(got.Files) != 1 || got.Files[0].FileName != "spec.pdf" {
		t.Fatalf("merged message = %+v", got)
	}
	if got.MessageID != "f1" && got.MessageID != "t1" {
		t.Fatalf("MessageID = %q", got.MessageID)
	}
	if got.SessionKey != SessionKey("corp1", "wk1", "u1") {
		t.Fatalf("SessionKey = %q", got.SessionKey)
	}
}

func TestMerge_WindowSeparatesTurnsAndCustomers(t *testing.T) {
	f := newFakeWeCom(t)
	p, rec := newMergingPlatform(t, f, 300)

	a1 := customerText("a1", "wk1", "alice", "first")
	a2 := customerText("a2", "wk1", "alice", "second")
	b1 := customerText("b1", "wk1", "bob", "bob's question")
	p.processMessage(p.ctx, &a1)
	p.processMessage(p.ctx, &b1)
	p.processMessage(p.ctx, &a2)

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

	// After the window a new message starts a new turn.
	a3 := customerText("a3", "wk1", "alice", "later")
	p.processMessage(p.ctx, &a3)
	if m := rec.wait(t); m.Content != "later" {
		t.Fatalf("new turn = %+v", m)
	}
}

func TestMerge_FailedDownloads(t *testing.T) {
	t.Run("only a failed download dispatches nothing", func(t *testing.T) {
		f := newFakeWeCom(t)
		p, rec := newMergingPlatform(t, f, 30)
		m := mediaMsg("x", "image", "missing", "u")
		p.processMessage(p.ctx, &m)
		rec.expectNone(t)
		time.Sleep(60 * time.Millisecond)
		rec.expectNone(t)
	})
	t.Run("failed download plus text dispatches the text", func(t *testing.T) {
		f := newFakeWeCom(t)
		gate := make(chan struct{})
		f.mediaGate = gate
		p, rec := newMergingPlatform(t, f, 30)
		img := mediaMsg("x", "image", "missing", "u")
		txt := customerText("t", "wk1", "u", "see picture")
		p.processMessage(p.ctx, &img)
		p.processMessage(p.ctx, &txt)
		time.Sleep(80 * time.Millisecond)
		close(gate)
		got := rec.wait(t)
		if got.Content != "see picture" || len(got.Images) != 0 {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("download finishing inside the window waits for it", func(t *testing.T) {
		f := newFakeWeCom(t)
		f.addMedia("img", fakeMedia{data: pngBytes})
		p, rec := newMergingPlatform(t, f, 80)
		img := mediaMsg("i", "image", "img", "u")
		p.processMessage(p.ctx, &img)
		got := rec.wait(t)
		if len(got.Images) != 1 || got.Images[0].MimeType != "image/png" {
			t.Fatalf("got %+v", got)
		}
	})
}

func TestMerge_StopDropsPendingMessages(t *testing.T) {
	f := newFakeWeCom(t)
	p, rec := newDirectPlatform(t, f, map[string]any{"merge_window_ms": int64(60_000)})
	m := customerText("t", "wk1", "u", "pending")
	p.processMessage(p.ctx, &m)
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	rec.expectNone(t)
	p.aggMu.Lock()
	n := len(p.agg)
	p.aggMu.Unlock()
	if n != 0 {
		t.Fatalf("%d aggregates left after Stop", n)
	}
	// A download completing after Stop must be ignored safely.
	p.complete(SessionKey("corp1", "wk1", "u"), &core.Message{Content: "late"})
	rec.expectNone(t)
}

func TestMergeInto(t *testing.T) {
	dst := &core.Message{Content: "a", UserMessageTimeMs: 5}
	audio := &core.AudioAttachment{Format: "amr"}
	mergeInto(dst, &core.Message{MessageID: "m2", Images: []core.ImageAttachment{{}}, Audio: audio, UserMessageTimeMs: 9})
	mergeInto(dst, &core.Message{MessageID: "m3", Content: "b", Files: []core.FileAttachment{{}}, UserMessageTimeMs: 7})
	if dst.Content != "a\nb" || len(dst.Images) != 1 || len(dst.Files) != 1 || dst.Audio != audio || dst.MessageID != "m3" || dst.UserMessageTimeMs != 9 {
		t.Fatalf("merged = %+v", dst)
	}
}

func TestNew_MergeWindowOption(t *testing.T) {
	base := map[string]any{"corp_id": "c", "corp_secret": "s", "open_kfid": "wk", "callback_token": "t", "callback_aes_key": testAESKeyEncoded}
	p, _ := New(base)
	if p.(*Platform).mergeWindow != defaultMergeWindow {
		t.Fatalf("default merge window = %v", p.(*Platform).mergeWindow)
	}
	base["merge_window_ms"] = int64(-5)
	p, _ = New(base)
	if p.(*Platform).mergeWindow != 0 {
		t.Fatalf("negative merge window = %v, want 0", p.(*Platform).mergeWindow)
	}
	base["merge_window_ms"] = float64(500)
	p, _ = New(base)
	if p.(*Platform).mergeWindow != 500*time.Millisecond {
		t.Fatalf("merge window = %v", p.(*Platform).mergeWindow)
	}
}

func TestMerge_StaleTimerIsIgnored(t *testing.T) {
	f := newFakeWeCom(t)
	p, rec := newMergingPlatform(t, f, 60_000)
	m := customerText("t", "wk1", "u", "current")
	p.processMessage(p.ctx, &m)

	// A timer belonging to an aggregate that was already flushed and
	// replaced must not dispatch (or delete) the current one.
	key := SessionKey("corp1", "wk1", "u")
	p.flush(key, &aggregate{msg: &core.Message{Content: "stale"}})
	rec.expectNone(t)
	p.aggMu.Lock()
	cur := p.agg[key]
	p.aggMu.Unlock()
	if cur == nil || cur.msg.Content != "current" {
		t.Fatalf("current aggregate disturbed: %+v", cur)
	}
	// The real timer path still works.
	p.flush(key, cur)
	if got := rec.wait(t); got.Content != "current" {
		t.Fatalf("got %+v", got)
	}
}
