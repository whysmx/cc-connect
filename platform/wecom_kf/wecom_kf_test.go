package wecom_kf

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/sha1"
	"fmt"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func testOptions(baseURL string) map[string]any {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x44}, 32))
	return map[string]any{
		"corp_id": "corp", "corp_secret": "secret", "callback_token": "token",
		"callback_aes_key": strings.TrimSuffix(key, "="), "open_kfid": "wk1",
		"api_base_url": baseURL, "listen_addr": "127.0.0.1:0",
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(map[string]any{}); err == nil {
		t.Fatal("New(empty) accepted")
	}
	p, err := New(testOptions("http://127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "wecom-kf" {
		t.Fatalf("Name() = %q", p.Name())
	}
}

func TestCallbackVerificationAndBadSignature(t *testing.T) {
	p, err := New(testOptions("http://127.0.0.1"))
	if err != nil { t.Fatal(err) }
	platform := p.(*Platform)
	event := []byte("<xml><ToUserName>corp</ToUserName><CreateTime>1700000000</CreateTime><MsgType>event</MsgType><Event>kf_msg_or_event</Event><Token>pull</Token><OpenKfId>wk1</OpenKfId></xml>")
	encrypted := encryptPlatformTest(t, event, "corp", platform.aesKey)
	sig := signPlatformTest(platform.callbackToken, "1", "nonce", encrypted)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/wecom-kf/callback?msg_signature="+sig+"&timestamp=1&nonce=nonce", strings.NewReader("<xml><Encrypt>"+encrypted+"</Encrypt></xml>"))
	platform.callbackHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("valid callback status = %d", rr.Code)
	}
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/wecom-kf/callback?msg_signature=bad&timestamp=1&nonce=nonce", strings.NewReader("<xml><Encrypt>"+encrypted+"</Encrypt></xml>"))
	platform.callbackHandler(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("bad callback status = %d", rr.Code)
	}
}

func TestPullAndReply(t *testing.T) {
	var mu sync.Mutex
	var got []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/cgi-bin/gettoken":
			_, _ = w.Write([]byte(`{"errcode":0,"access_token":"access","expires_in":7200}`))
		case "/cgi-bin/kf/sync_msg":
			_, _ = w.Write([]byte(`{"errcode":0,"next_cursor":"next","has_more":false,"msg_list":[{"msgid":"m1","open_kfid":"wk1","external_userid":"u1","origin":3,"msgtype":"text","send_time":1700000000,"text":{"content":"hello"}}]}`))
		case "/cgi-bin/kf/send_msg":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock(); got = append(got, body["msgtype"].(string)); mu.Unlock()
			_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	p, err := New(testOptions(api.URL))
	if err != nil { t.Fatal(err) }
	platform := p.(*Platform)
	var received *core.Message
	platform.handler = func(_ core.Platform, msg *core.Message) { received = msg }
	platform.pullMessages("pull", "wk1")
	if received == nil || received.Content != "hello" || received.SessionKey != "wecom-kf:wk1:u1" {
		t.Fatalf("unexpected message: %+v", received)
	}
	if err := platform.Reply(context.Background(), received.ReplyCtx, "answer"); err != nil {
		t.Fatal(err)
	}
	mu.Lock(); defer mu.Unlock()
	if len(got) != 1 || got[0] != "text" {
		t.Fatalf("sent messages: %#v", got)
	}
}

func encryptPlatformTest(t *testing.T, message []byte, receiveID string, key []byte) string {
	t.Helper()
	block, err := aes.NewCipher(key); if err != nil { t.Fatal(err) }
	body := append(bytes.Repeat([]byte{0x55}, 16), make([]byte, 4)...)
	binary.BigEndian.PutUint32(body[16:20], uint32(len(message)))
	body = append(body, message...); body = append(body, receiveID...)
	pad := 32 - len(body)%32
	body = append(body, bytes.Repeat([]byte{byte(pad)}, pad)...)
	out := make([]byte, len(body))
	cipher.NewCBCEncrypter(block, key[:aes.BlockSize]).CryptBlocks(out, body)
	return base64.StdEncoding.EncodeToString(out)
}

func signPlatformTest(token, timestamp, nonce, encrypted string) string {
	values := []string{token, timestamp, nonce, encrypted}
	sortStrings(values)
	return sha1Hex(strings.Join(values, ""))
}

func sortStrings(values []string) { for i := range values { for j := i + 1; j < len(values); j++ { if values[j] < values[i] { values[i], values[j] = values[j], values[i] } } } }
func sha1Hex(s string) string { h := sha1.Sum([]byte(s)); return fmt.Sprintf("%x", h[:]) }



func TestPullStopsOnUnchangedCursor(t *testing.T) {
	var syncCalls int
	var mu sync.Mutex
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/cgi-bin/gettoken":
			_, _ = w.Write([]byte(`{"errcode":0,"access_token":"access","expires_in":7200}`))
		case "/cgi-bin/kf/sync_msg":
			mu.Lock(); syncCalls++; mu.Unlock()
			_, _ = w.Write([]byte(`{"errcode":0,"next_cursor":"","has_more":true,"msg_list":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	p, err := New(testOptions(api.URL))
	if err != nil { t.Fatal(err) }
	p.(*Platform).pullMessages("pull", "wk1")
	mu.Lock(); calls := syncCalls; mu.Unlock()
	if calls != 1 {
		t.Fatalf("sync calls = %d, want 1", calls)
	}
}

func TestPullMessagesSerializesConcurrentCallbacks(t *testing.T) {
	var mu sync.Mutex
	active, maxActive := 0, 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/cgi-bin/gettoken":
			_, _ = w.Write([]byte(`{"errcode":0,"access_token":"access","expires_in":7200}`))
		case "/cgi-bin/kf/sync_msg":
			mu.Lock(); active++; if active > maxActive { maxActive = active }; mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			mu.Lock(); active--; mu.Unlock()
			_, _ = w.Write([]byte(`{"errcode":0,"next_cursor":"done","has_more":false,"msg_list":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	p, err := New(testOptions(api.URL))
	if err != nil { t.Fatal(err) }
	platform := p.(*Platform)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); platform.pullMessages("pull", "wk1") }()
	go func() { defer wg.Done(); platform.pullMessages("pull", "wk1") }()
	wg.Wait()
	mu.Lock(); got := maxActive; mu.Unlock()
	if got != 1 {
		t.Fatalf("maximum concurrent sync requests = %d, want 1", got)
	}
}


func TestCursorTag(t *testing.T) {
	if got := cursorTag(""); got != "empty" { t.Fatalf("empty cursor tag = %q", got) }
	if got := cursorTag("secret-cursor"); len(got) != 8 { t.Fatalf("cursor tag length = %d", len(got)) }
}
