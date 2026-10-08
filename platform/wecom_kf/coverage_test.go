package wecom_kf

// These tests intentionally exercise the adapter's HTTP boundary and lifecycle.  They
// complement the protocol/client tests in platform/wecomkf and are written without a
// real WeCom account, using httptest servers for every external request.

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestCoverageNewDefaultsAndInvalidOptions(t *testing.T) {
	if _, err := New(map[string]any{}); err == nil {
		t.Fatal("empty options should fail")
	}
	bad := testOptions("http://127.0.0.1")
	bad["callback_aes_key"] = "not-a-key"
	if _, err := New(bad); err == nil {
		t.Fatal("invalid AES key should fail")
	}
	defaults := testOptions("")
	delete(defaults, "api_base_url")
	delete(defaults, "listen_addr")
	p, err := New(defaults)
	if err != nil {
		t.Fatal(err)
	}
	got := p.(*Platform)
	if got.apiBaseURL != "https://qyapi.weixin.qq.com" || got.callbackPath != "/wecom-kf/callback" || got.listenAddr != ":8081" {
		t.Fatalf("defaults not applied: base=%q path=%q addr=%q", got.apiBaseURL, got.callbackPath, got.listenAddr)
	}
	custom := testOptions("http://example.test/")
	custom["callback_path"], custom["listen_addr"] = "/hook", "127.0.0.1:12345"
	custom["allow_from"] = "u1,u2"
	p2, err := New(custom)
	if err != nil {
		t.Fatal(err)
	}
	got2 := p2.(*Platform)
	if got2.apiBaseURL != "http://example.test" || got2.callbackPath != "/hook" || got2.listenAddr != "127.0.0.1:12345" || got2.allowFrom != "u1,u2" {
		t.Fatalf("custom options lost: %+v", got2)
	}
}

func TestCoverageCallbackAllBranches(t *testing.T) {
	p0, err := New(testOptions("http://127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	p := p0.(*Platform)
	// GET verification success.
	echo := encryptAdapterCoverage(t, []byte("verified"), p.corpID, p.aesKey)
	sig := signPlatformTest(p.callbackToken, "10", "n", echo)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/?msg_signature="+sig+"&timestamp=10&nonce=n&echostr="+url.QueryEscape(echo), nil)
	p.callbackHandler(rr, req)
	if rr.Code != http.StatusOK || rr.Body.String() != "verified" {
		t.Fatalf("GET verify: status=%d body=%q", rr.Code, rr.Body.String())
	}
	for _, tc := range []struct {
		name, query string
		want int
	}{
		{"missing echo", "msg_signature=" + sig + "&timestamp=10&nonce=n", http.StatusForbidden},
		{"bad signature", "msg_signature=nope&timestamp=10&nonce=n&echostr=" + url.QueryEscape(echo), http.StatusForbidden},
		{"bad ciphertext", "msg_signature=" + signPlatformTest(p.callbackToken, "10", "n", "bad") + "&timestamp=10&nonce=n&echostr=bad", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRecorder()
			p.callbackHandler(r, httptest.NewRequest(http.MethodGet, "/?"+tc.query, nil))
			if r.Code != tc.want {
				t.Fatalf("status=%d want=%d", r.Code, tc.want)
			}
		})
	}
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		r := httptest.NewRecorder()
		p.callbackHandler(r, httptest.NewRequest(method, "/", nil))
		if r.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s status=%d", method, r.Code)
		}
	}
	post := func(body, query string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		p.callbackHandler(r, httptest.NewRequest(http.MethodPost, "/?"+query, strings.NewReader(body)))
		return r
	}
	if r := post("", ""); r.Code != http.StatusBadRequest {
		t.Fatalf("empty POST status=%d", r.Code)
	}
	if r := post("<not-xml", ""); r.Code != http.StatusForbidden {
		t.Fatalf("malformed XML status=%d", r.Code)
	}
	if r := post("<xml><Encrypt>"+echo+"</Encrypt></xml>", "msg_signature=nope&timestamp=10&nonce=n"); r.Code != http.StatusForbidden {
		t.Fatalf("bad POST signature status=%d", r.Code)
	}
	validQuery := "msg_signature=" + sig + "&timestamp=10&nonce=n"
	badCipherSig := signPlatformTest(p.callbackToken, "10", "n", "bad")
	if r := post("<xml><Encrypt>bad</Encrypt></xml>", "msg_signature="+badCipherSig+"&timestamp=10&nonce=n"); r.Code != http.StatusBadRequest {
		t.Fatalf("bad POST decrypt status=%d", r.Code)
	}
	badEvent := encryptAdapterCoverage(t, []byte("<xml><Event>wrong</Event></xml>"), p.corpID, p.aesKey)
	if r := post("<xml><Encrypt>"+badEvent+"</Encrypt></xml>", "msg_signature="+signPlatformTest(p.callbackToken, "10", "n", badEvent)+"&timestamp=10&nonce=n"); r.Code != http.StatusBadRequest {
		t.Fatalf("bad event status=%d", r.Code)
	}
	event := []byte("<xml><Event>kf_msg_or_event</Event><Token>pull</Token><OpenKfId>other</OpenKfId></xml>")
	wrongKf := encryptAdapterCoverage(t, event, p.corpID, p.aesKey)
	if r := post("<xml><Encrypt>"+wrongKf+"</Encrypt></xml>", "msg_signature="+signPlatformTest(p.callbackToken, "10", "n", wrongKf)+"&timestamp=10&nonce=n"); r.Code != http.StatusBadRequest {
		t.Fatalf("wrong open_kfid status=%d", r.Code)
	}
}

func TestCoveragePullPaginationFilteringAndDedup(t *testing.T) {
	var mu sync.Mutex
	var tokenCalls, syncCalls int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/cgi-bin/gettoken":
			tokenCalls++
			_, _ = w.Write([]byte(`{"errcode":0,"access_token":"access","expires_in":7200}`))
		case "/cgi-bin/kf/sync_msg":
			syncCalls++
			if syncCalls == 1 {
				_, _ = w.Write([]byte(`{"errcode":0,"next_cursor":"next","has_more":1,"msg_list":[
{"msgid":"m1","open_kfid":"wk1","external_userid":"u1","origin":3,"msgtype":"text","send_time":1700000000,"text":{"content":"hello"}},
{"msgid":"m1","open_kfid":"wk1","external_userid":"u1","origin":3,"msgtype":"text","text":{"content":"duplicate"}},
{"msgid":"ignored-origin","open_kfid":"wk1","external_userid":"u1","origin":1,"msgtype":"text","text":{"content":"x"}},
{"msgid":"ignored-type","open_kfid":"wk1","external_userid":"u1","origin":3,"msgtype":"image"},
{"msgid":"ignored-text","open_kfid":"wk1","external_userid":"u1","origin":3,"msgtype":"text"},
{"msgid":"ignored-user","open_kfid":"wk1","origin":3,"msgtype":"text","text":{"content":"x"}}]}`))
			} else {
				_, _ = w.Write([]byte(`{"errcode":0,"next_cursor":"done","has_more":0,"msg_list":[{"msgid":"m2","open_kfid":"wk1","external_userid":"u2","origin":3,"msgtype":"text","text":{"content":"world"}}]}`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	opts := testOptions(api.URL)
	opts["allow_from"] = "u1,u2"
	p0, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	p := p0.(*Platform)
	var got []string
	p.handler = func(_ core.Platform, m *core.Message) { got = append(got, m.Content) }
	p.pullMessages("pull", "wk1")
	if len(got) != 2 || got[0] != "hello" || got[1] != "world" {
		t.Fatalf("filtered messages: %#v", got)
	}
	if p.cursor != "done" || syncCalls != 2 || tokenCalls != 1 {
		t.Fatalf("pagination state cursor=%q sync=%d token=%d", p.cursor, syncCalls, tokenCalls)
	}
	// A second pull receives the same IDs and exercises the de-duplication cache.
	p.pullMessages("pull", "wk1")
	if len(got) != 2 {
		t.Fatalf("duplicate messages delivered: %#v", got)
	}
	if tok, err := p.accessToken(context.Background()); err != nil || tok != "access" || tokenCalls != 1 {
		t.Fatalf("cached token: %q err=%v calls=%d", tok, err, tokenCalls)
	}
}

func TestCoverageTokenAndReplyErrors(t *testing.T) {
	badAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/cgi-bin/gettoken":
			_, _ = w.Write([]byte(`{"errcode":40013,"errmsg":"bad corp"}`))
		case "/cgi-bin/kf/send_msg":
			_, _ = w.Write([]byte(`{"errcode":40014,"errmsg":"bad token"}`))
		}
	}))
	defer badAPI.Close()
	p0, err := New(testOptions(badAPI.URL))
	if err != nil {
		t.Fatal(err)
	}
	p := p0.(*Platform)
	if _, err := p.accessToken(context.Background()); err == nil {
		t.Fatal("API token error swallowed")
	}
	if err := p.Reply(context.Background(), "wrong", "hello"); err == nil {
		t.Fatal("invalid reply context accepted")
	}
	if err := p.Send(context.Background(), "wrong", "hello"); err == nil {
		t.Fatal("invalid send context accepted")
	}
	if err := p.Reply(context.Background(), replyContext{toUser: "u", openKfID: "wk", msgID: "m"}, "hello"); err == nil {
		t.Fatal("token failure hidden by Reply")
	}
	// A canceled context exercises request creation/transport failure without a server.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.accessToken(ctx); err == nil {
		t.Fatal("canceled token request unexpectedly succeeded")
	}
	// Expired cache values are refreshed; this also covers the cache miss branch.
	p.token.value, p.token.expiresAt = "stale", time.Now().Add(-time.Hour)
	if _, err := p.accessToken(context.Background()); err == nil {
		t.Fatal("expired cache should refresh and fail against bad API")
	}
}

func TestCoverageReplyChunkingSendAndLifecycle(t *testing.T) {
	var mu sync.Mutex
	var chunks []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/cgi-bin/gettoken":
			_, _ = w.Write([]byte(`{"errcode":0,"access_token":"access","expires_in":7200}`))
		case "/cgi-bin/kf/send_msg":
			var body struct{ Text struct{ Content string `json:"content"` } `json:"text"` }
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			chunks = append(chunks, body.Text.Content)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"errcode":0}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	p0, err := New(testOptions(api.URL))
	if err != nil {
		t.Fatal(err)
	}
	p := p0.(*Platform)
	content := strings.Repeat("a", 2050)
	if err := p.Send(context.Background(), replyContext{toUser: "u", openKfID: "wk", msgID: "m"}, content); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(chunks) != 2 || len(chunks[0]) != 2048 || len(chunks[1]) != 2 {
		t.Fatalf("chunk sizes: %#v", chunks)
	}
	mu.Unlock()
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := p.Start(nil); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
}

// encryptAdapterCoverage mirrors WeCom's callback framing. In particular, the
// CBC IV is the first AES block of the decoded encoding key (key[:16]).
func encryptAdapterCoverage(t *testing.T, message []byte, receiveID string, key []byte) string {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	body := append(bytes.Repeat([]byte{0x55}, 16), make([]byte, 4)...)
	binary.BigEndian.PutUint32(body[16:20], uint32(len(message)))
	body = append(body, message...)
	body = append(body, receiveID...)
	pad := 32 - len(body)%32
	body = append(body, bytes.Repeat([]byte{byte(pad)}, pad)...)
	out := make([]byte, len(body))
	cipher.NewCBCEncrypter(block, key[:aes.BlockSize]).CryptBlocks(out, body)
	return base64.StdEncoding.EncodeToString(out)
}

