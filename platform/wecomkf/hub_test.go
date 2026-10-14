package wecomkf

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// newTestHub builds a hub with one route without opening a listener so the
// HTTP handler can be exercised directly.
func newTestHub(t *testing.T, p *Platform) *hub {
	t.Helper()
	return &hub{routes: map[string]*route{p.callbackPath: newRoute(p)}}
}

func signedQuery(token, encrypted string) string {
	sig := computeSignature(token, "1700000000", "n1", encrypted)
	return "msg_signature=" + sig + "&timestamp=1700000000&nonce=n1"
}

func serveHub(h *hub, method, target, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, strings.NewReader(body)))
	return rec
}

func TestHub_RejectsBadCallbacks(t *testing.T) {
	f := newFakeWeCom(t)
	p := newTestPlatform(t, f, platformOpts{kfID: "wk1", path: "/cb"})
	h := newTestHub(t, p)

	goodInner := "<xml><Event>kf_msg_or_event</Event><Token>t</Token><OpenKfId>wk1</OpenKfId></xml>"
	encGood := encryptForTest(t, p.aesKey, goodInner, "corp1")
	encOtherCorp := encryptForTest(t, p.aesKey, goodInner, "corp2")
	encNotXML := encryptForTest(t, p.aesKey, "<xml", "corp1")
	envelope := func(enc string) string { return "<xml><Encrypt>" + enc + "</Encrypt></xml>" }

	cases := []struct {
		name, method, target, body string
		want                       int
	}{
		{"unknown path", http.MethodPost, "/other", "", http.StatusNotFound},
		{"method not allowed", http.MethodPut, "/cb", "", http.StatusMethodNotAllowed},
		{"empty body", http.MethodPost, "/cb?" + signedQuery(p.callbackToken, ""), "", http.StatusBadRequest},
		{"malformed xml", http.MethodPost, "/cb", "<xml", http.StatusBadRequest},
		{"missing Encrypt", http.MethodPost, "/cb", "<xml><ToUserName>x</ToUserName></xml>", http.StatusBadRequest},
		{"bad signature", http.MethodPost, "/cb?msg_signature=00&timestamp=1&nonce=n", envelope(encGood), http.StatusForbidden},
		{"wrong receive id", http.MethodPost, "/cb?" + signedQuery(p.callbackToken, encOtherCorp), envelope(encOtherCorp), http.StatusBadRequest},
		{"decrypted body not xml", http.MethodPost, "/cb?" + signedQuery(p.callbackToken, encNotXML), envelope(encNotXML), http.StatusBadRequest},
		{"verify decrypt fails", http.MethodGet, "/cb?echostr=" + url.QueryEscape(encOtherCorp) + "&" + signedQuery(p.callbackToken, encOtherCorp), "", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rec := serveHub(h, tc.method, tc.target, tc.body); rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
	if len(f.syncRequests()) != 0 {
		t.Fatal("rejected callbacks must not trigger a pull")
	}
}

func TestHub_AcknowledgesButIgnoresUnroutableEvents(t *testing.T) {
	f := newFakeWeCom(t)
	p := newTestPlatform(t, f, platformOpts{kfID: "wk1", path: "/cb"})
	h := newTestHub(t, p)

	for name, inner := range map[string]string{
		"other event":       "<xml><Event>change_contact</Event><Token>t</Token><OpenKfId>wk1</OpenKfId></xml>",
		"unknown open_kfid": "<xml><Event>kf_msg_or_event</Event><Token>t</Token><OpenKfId>wk-unknown</OpenKfId></xml>",
		"empty open_kfid":   "<xml><Event>kf_msg_or_event</Event><Token>t</Token></xml>",
	} {
		t.Run(name, func(t *testing.T) {
			enc := encryptForTest(t, p.aesKey, inner, "corp1")
			rec := serveHub(h, http.MethodPost, "/cb?"+signedQuery(p.callbackToken, enc), "<xml><Encrypt>"+enc+"</Encrypt></xml>")
			if rec.Code != http.StatusOK || rec.Body.String() != "success" {
				t.Fatalf("status=%d body=%q, want 200 success", rec.Code, rec.Body.String())
			}
		})
	}
	if len(f.syncRequests()) != 0 {
		t.Fatal("unroutable events must not trigger a pull")
	}
	if h.account("/missing", "wk1") != nil {
		t.Fatal("account lookup on a missing route must return nil")
	}
}

func TestRegisterPlatform_ListenErrorAndSeparatePaths(t *testing.T) {
	f := newFakeWeCom(t)

	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	p := newTestPlatform(t, f, platformOpts{kfID: "wk1", listen: busy.Addr().String(), path: "/busy"})
	if err := p.Start(newRecorder().handle); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("Start on a busy port err = %v", err)
	}
	if lookupHub(busy.Addr().String()) != nil {
		t.Fatal("failed listener must not be registered")
	}

	// Two callback paths on one listener (e.g. two corps) get separate routes.
	listen := "127.0.0.1:0"
	pa := newTestPlatform(t, f, platformOpts{kfID: "wkA", listen: listen, path: "/corp-a"})
	pb := newTestPlatform(t, f, platformOpts{kfID: "wkB", listen: listen, path: "/corp-b", token: "other-token"})
	startPlatform(t, pa, newRecorder())
	startPlatform(t, pb, newRecorder())
	h := lookupHub(listen)
	if h.route("/corp-a") == nil || h.route("/corp-b") == nil || h.route("/corp-b").callbackToken != "other-token" {
		t.Fatal("expected two independent routes on one listener")
	}

	// Stopping everything shuts the shared listener down; stopping twice is harmless.
	if err := pa.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := pb.Stop(); err != nil {
		t.Fatal(err)
	}
	if lookupHub(listen) != nil {
		t.Fatal("listener must be closed after the last account stops")
	}
	if err := pb.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}
