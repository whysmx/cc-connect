package wecomkf

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{context.Canceled, false},
		{&apiError{Op: "x", ErrCode: -1}, true},
		{&apiError{Op: "x", ErrCode: 42001}, true},
		{&apiError{Op: "x", ErrCode: 95018}, false},
		{errors.New("connection reset"), true},
	}
	for _, tc := range cases {
		if got := isRetryable(tc.err); got != tc.want {
			t.Errorf("isRetryable(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func newRawServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestAccessToken_ErrorsAndDefaults(t *testing.T) {
	t.Run("api error", func(t *testing.T) {
		srv := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"errcode":40013,"errmsg":"invalid corpid"}`))
		})
		_, err := newAPIClient(srv.URL, "c", "s", srv.Client()).accessToken(context.Background(), false)
		var ae *apiError
		if !errors.As(err, &ae) || ae.ErrCode != 40013 || !strings.Contains(err.Error(), "gettoken") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("malformed json", func(t *testing.T) {
		srv := newRawServer(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<html>")) })
		if _, err := newAPIClient(srv.URL, "c", "s", srv.Client()).accessToken(context.Background(), false); err == nil {
			t.Fatal("expected decode error")
		}
	})
	t.Run("network error redacts secret", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		_, err := newAPIClient(url, "c", "top-secret", http.DefaultClient).accessToken(context.Background(), false)
		if err == nil || strings.Contains(err.Error(), "top-secret") {
			t.Fatalf("err = %v (secret must be redacted)", err)
		}
	})
	t.Run("missing expires_in uses default and caches", func(t *testing.T) {
		calls := 0
		srv := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
			calls++
			_, _ = w.Write([]byte(`{"errcode":0,"access_token":"tok"}`))
		})
		c := newAPIClient(srv.URL, "c", "s", srv.Client())
		for i := 0; i < 3; i++ {
			if tok, err := c.accessToken(context.Background(), false); err != nil || tok != "tok" {
				t.Fatalf("accessToken = %q, %v", tok, err)
			}
		}
		if calls != 1 || time.Until(c.expiresAt) < time.Hour {
			t.Fatalf("calls=%d expiresIn=%v; want cached token with ~2h lifetime", calls, time.Until(c.expiresAt))
		}
		if _, err := c.accessToken(context.Background(), true); err != nil || calls != 2 {
			t.Fatalf("forced refresh: calls=%d err=%v", calls, err)
		}
	})
	t.Run("default base url", func(t *testing.T) {
		if c := newAPIClient("  ", "c", "s", nil); c.baseURL != defaultAPIBaseURL {
			t.Fatalf("baseURL = %q", c.baseURL)
		}
	})
}

func TestPostJSON_Failures(t *testing.T) {
	tokenOK := func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/cgi-bin/gettoken" {
			_, _ = w.Write([]byte(`{"errcode":0,"access_token":"t","expires_in":7200}`))
			return true
		}
		return false
	}
	t.Run("token keeps expiring", func(t *testing.T) {
		srv := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
			if !tokenOK(w, r) {
				_, _ = w.Write([]byte(`{"errcode":42001,"errmsg":"expired"}`))
			}
		})
		_, _, err := newAPIClient(srv.URL, "c", "s", srv.Client()).serviceState(context.Background(), "wk", "u")
		var ae *apiError
		if !errors.As(err, &ae) || ae.ErrCode != 42001 {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("malformed response", func(t *testing.T) {
		srv := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
			if !tokenOK(w, r) {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte("bad gateway"))
			}
		})
		_, err := newAPIClient(srv.URL, "c", "s", srv.Client()).syncMsg(context.Background(), syncMsgRequest{OpenKfID: "wk"})
		if err == nil || !strings.Contains(err.Error(), "http 502") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("gettoken failure propagates", func(t *testing.T) {
		srv := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"errcode":40001,"errmsg":"bad secret"}`))
		})
		if _, err := newAPIClient(srv.URL, "c", "s", srv.Client()).sendText(context.Background(), "wk", "u", "x"); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("unencodable body", func(t *testing.T) {
		c := newAPIClient("http://127.0.0.1:1", "c", "s", http.DefaultClient)
		if err := c.postJSON(context.Background(), "op", "/p", map[string]any{"x": make(chan int)}, &baseResponse{}); err == nil {
			t.Fatal("expected encode error")
		}
	})
	t.Run("network error on post", func(t *testing.T) {
		srv := newRawServer(t, func(w http.ResponseWriter, r *http.Request) {
			if !tokenOK(w, r) {
				// Drop the connection to simulate a network failure.
				hj, _ := w.(http.Hijacker)
				conn, _, _ := hj.Hijack()
				_ = conn.Close()
			}
		})
		if _, err := newAPIClient(srv.URL, "c", "s", srv.Client()).sendText(context.Background(), "wk", "u", "x"); err == nil || !isRetryable(err) {
			t.Fatalf("err = %v, want retryable network error", err)
		}
	})
}

func TestDescribeStateAndAIMayReply(t *testing.T) {
	want := map[int]struct {
		name  string
		allow bool
	}{
		stateUnhandled:  {"unhandled", true},
		stateAssistant:  {"assistant", true},
		stateQueued:     {"queued_for_human", false},
		stateHuman:      {"human", false},
		stateEndedOrNew: {"ended", false},
		9:               {"unknown(9)", false},
	}
	for state, w := range want {
		if got := describeState(state); got != w.name {
			t.Errorf("describeState(%d) = %q, want %q", state, got, w.name)
		}
		if got := aiMayReply(state); got != w.allow {
			t.Errorf("aiMayReply(%d) = %v, want %v", state, got, w.allow)
		}
	}
}
