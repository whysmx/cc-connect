package wecomkf

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

var pngBytes = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)

func mediaMsg(msgID, kind, mediaID, user string) kfMessage {
	m := kfMessage{MsgID: msgID, OpenKfID: "wk1", ExternalUserID: user, SendTime: time.Now().Unix(), Origin: originCustomer, MsgType: kind}
	md := &kfMedia{MediaID: mediaID}
	switch kind {
	case "image":
		m.Image = md
	case "voice":
		m.Voice = md
	case "video":
		m.Video = md
	case "file":
		m.File = md
	}
	return m
}

func TestInboundMedia_AllTypesReachTheEngine(t *testing.T) {
	f := newFakeWeCom(t)
	f.addMedia("img", fakeMedia{data: pngBytes, contentType: "application/octet-stream"})
	f.addMedia("amr", fakeMedia{data: []byte("#!AMR\n..."), contentType: "voice/amr"})
	f.addMedia("vid", fakeMedia{data: []byte("not-sniffable-video"), contentType: "application/octet-stream"})
	f.addMedia("pdf", fakeMedia{data: []byte("%PDF-1.7"), contentType: "application/octet-stream",
		disposition: `attachment; filename*=UTF-8''%E6%8A%A5%E5%91%8A.pdf`})

	p := newTestPlatform(t, f, platformOpts{kfID: "wk1", path: "/kf-media"})
	rec := newRecorder()
	startPlatform(t, p, rec)
	f.addPage("wk1", syncMsgResponse{NextCursor: "c1", MsgList: []kfMessage{
		mediaMsg("i1", "image", "img", "u1"),
		mediaMsg("v1", "voice", "amr", "u2"),
		mediaMsg("d1", "video", "vid", "u3"),
		mediaMsg("f1", "file", "pdf", "u4"),
	}})
	postNotify(t, p, "wk1", "tok")

	got := map[string]*core.Message{}
	for i := 0; i < 4; i++ {
		m := rec.wait(t)
		got[m.MessageID] = m
	}
	if img := got["i1"]; len(img.Images) != 1 || img.Images[0].MimeType != "image/png" || len(img.Images[0].Data) != len(pngBytes) {
		t.Fatalf("image message = %+v", img)
	}
	if v := got["v1"]; v.Audio == nil || v.Audio.Format != "amr" || v.Audio.MimeType != "audio/amr" || string(v.Audio.Data) != "#!AMR\n..." {
		t.Fatalf("voice message audio = %+v", v.Audio)
	}
	if d := got["d1"]; len(d.Files) != 1 || d.Files[0].FileName != "video.mp4" || d.Files[0].MimeType != "video/mp4" {
		t.Fatalf("video message files = %+v", d.Files)
	}
	if fm := got["f1"]; len(fm.Files) != 1 || fm.Files[0].FileName != "报告.pdf" || fm.Files[0].MimeType != "application/pdf" {
		t.Fatalf("file message files = %+v", fm.Files)
	}
	for id, m := range got {
		if m.SessionKey != SessionKey("corp1", "wk1", m.UserID) || m.ReplyCtx.(replyContext).msgID != id {
			t.Fatalf("routing fields wrong for %s: %+v", id, m)
		}
	}
}

func TestInboundMedia_FailuresAndDisabled(t *testing.T) {
	t.Run("download error is not dispatched", func(t *testing.T) {
		f := newFakeWeCom(t)
		p, rec := newDirectPlatform(t, f, nil)
		m := mediaMsg("bad", "image", "missing", "u")
		p.processMessage(p.ctx, &m)
		rec.expectNone(t)
		p.wg.Wait()
		if !p.store.MarkSeen("bad") {
			t.Fatal("failed media message must still be marked as processed")
		}
	})
	t.Run("inbound_media=false ignores media", func(t *testing.T) {
		f := newFakeWeCom(t)
		f.addMedia("img", fakeMedia{data: pngBytes})
		p, rec := newDirectPlatform(t, f, map[string]any{"inbound_media": false})
		m := mediaMsg("i", "image", "img", "u")
		p.processMessage(p.ctx, &m)
		rec.expectNone(t)
		f.mu.Lock()
		calls := f.mediaCalls
		f.mu.Unlock()
		if calls != 0 {
			t.Fatalf("media/get called %d times with inbound_media=false", calls)
		}
	})
	t.Run("human takeover skips the download", func(t *testing.T) {
		f := newFakeWeCom(t)
		f.addMedia("img", fakeMedia{data: pngBytes})
		f.setState("u", stateHuman)
		p, rec := newDirectPlatform(t, f, nil)
		m := mediaMsg("i", "image", "img", "u")
		p.processMessage(p.ctx, &m)
		rec.expectNone(t)
		f.mu.Lock()
		calls := f.mediaCalls
		f.mu.Unlock()
		if calls != 0 {
			t.Fatal("media downloaded although a human handles the session")
		}
	})
}

func TestDownloadMedia_ErrorsAndTokenRefresh(t *testing.T) {
	ctx := context.Background()
	t.Run("refreshes expired token", func(t *testing.T) {
		calls := 0
		srv := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/cgi-bin/gettoken" {
				_, _ = w.Write([]byte(`{"errcode":0,"access_token":"t","expires_in":7200}`))
				return
			}
			calls++
			if calls == 1 {
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte(`{"errcode":42001,"errmsg":"expired"}`))
				return
			}
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte{0xFF, 0xD8, 0xFF, 0x00})
		})
		md, err := newAPIClient(srv.URL, "c", "s", srv.Client()).downloadMedia(ctx, "m", 1024)
		if err != nil || len(md.Data) != 4 || md.ContentType != "image/jpeg" || calls != 2 {
			t.Fatalf("md=%+v err=%v calls=%d", md, err, calls)
		}
	})
	cases := map[string]struct {
		handler http.HandlerFunc
		want    string
	}{
		"api error": {func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"errcode":40007,"errmsg":"invalid media_id"}`))
		}, "40007"},
		"token keeps expiring": {func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"errcode":42001,"errmsg":"expired"}`))
		}, "42001"},
		"malformed error json": {func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"errcode": oops`))
		}, "decode error response"},
		"http error": {func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("boom"))
		}, "http 500"},
		"too large": {func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(make([]byte, 2048))
		}, "exceeds 1024 bytes"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/cgi-bin/gettoken" {
					_, _ = w.Write([]byte(`{"errcode":0,"access_token":"t","expires_in":7200}`))
					return
				}
				tc.handler(w, r)
			})
			_, err := newAPIClient(srv.URL, "c", "s", srv.Client()).downloadMedia(ctx, "m", 1024)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	t.Run("gettoken failure", func(t *testing.T) {
		srv := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"errcode":40001,"errmsg":"bad"}`))
		})
		var ae *apiError
		if _, err := newAPIClient(srv.URL, "c", "s", srv.Client()).downloadMedia(ctx, "m", 1024); !errors.As(err, &ae) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("network error", func(t *testing.T) {
		srv := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/cgi-bin/gettoken" {
				_, _ = w.Write([]byte(`{"errcode":0,"access_token":"t","expires_in":7200}`))
				return
			}
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		})
		if _, err := newAPIClient(srv.URL, "c", "s", srv.Client()).downloadMedia(ctx, "m", 1024); err == nil || !strings.Contains(err.Error(), "media/get") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestDispositionFileName(t *testing.T) {
	cases := map[string]string{
		"":                                           "",
		"attachment; filename=":                      "",
		"not a; valid=header=":                       "",
		`attachment; filename="report.pdf"`:          "report.pdf",
		`attachment; filename="a/b/../c.txt"`:        "c.txt",
		`attachment; filename="C:\\dir\\y.doc"`:      "y.doc",
		`attachment; filename="%E6%8A%A5.pdf"`:       "报.pdf",
		"attachment; filename*=UTF-8''%E5%9B%BE.png": "图.png",
		`attachment; filename=".."`:                  "",
	}
	for header, want := range cases {
		if got := dispositionFileName(header); got != want {
			t.Errorf("dispositionFileName(%q) = %q, want %q", header, got, want)
		}
	}
}

func TestDetectMimeAndClassify(t *testing.T) {
	cases := []struct {
		media    downloadedMedia
		fallback string
		want     string
	}{
		{downloadedMedia{FileName: "a.pdf"}, "x/y", "application/pdf"},
		{downloadedMedia{FileName: "a.unknownext", ContentType: "image/gif"}, "x/y", "image/gif"},
		{downloadedMedia{ContentType: "application/octet-stream", Data: pngBytes}, "x/y", "image/png"},
		{downloadedMedia{ContentType: "text/plain", Data: []byte("hello")}, "x/y", "x/y"},
		{downloadedMedia{}, "video/mp4", "video/mp4"},
	}
	for i, tc := range cases {
		if got := detectMime(&tc.media, tc.fallback); got != tc.want {
			t.Errorf("case %d: detectMime = %q, want %q", i, got, tc.want)
		}
	}

	for _, tc := range []struct {
		m        kfMessage
		wantKind string
	}{
		{kfMessage{MsgType: "text", Text: &kfText{Content: "hi"}}, "text"},
		{kfMessage{MsgType: "text"}, ""},
		{kfMessage{MsgType: "image", Image: &kfMedia{}}, ""},
		{kfMessage{MsgType: "image"}, ""},
		{kfMessage{MsgType: "file", File: &kfMedia{MediaID: "m"}}, "file"},
		{kfMessage{MsgType: "location"}, ""},
	} {
		if kind, _ := classifyMessage(&tc.m); kind != tc.wantKind {
			t.Errorf("classifyMessage(%s) = %q, want %q", tc.m.MsgType, kind, tc.wantKind)
		}
	}
}
