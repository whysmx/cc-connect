package wecomkf

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPIClientValidationAndResponseEdges(t *testing.T) {
	client := NewClient("", nil)
	if client.baseURL != defaultAPIBaseURL || client.httpClient == nil { t.Fatal("defaults not applied") }
	if err := client.SendText(context.Background(), "", SendTextRequest{}); err == nil { t.Fatal("empty token/request accepted") }
	if err := client.SendText(context.Background(), "token", SendTextRequest{ToUser: "u", OpenKfID: "wk"}); err == nil { t.Fatal("empty text accepted") }
	if _, err := client.SyncMessages(context.Background(), "", SyncMessageRequest{}); err == nil { t.Fatal("empty token accepted") }

	for _, tc := range []struct{name, body string; status int; want string}{
		{"http", "ok", http.StatusBadGateway, "HTTP 502"},
		{"json", "not-json", http.StatusOK, "decode"},
		{"api", `{"errcode":95002,"errmsg":"expired"}`, http.StatusOK, "API error 95002"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status); _, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			err := NewClient(srv.URL, srv.Client()).SendText(context.Background(), "token", SendTextRequest{ToUser: "u", OpenKfID: "wk", Text: MessageText{Content: "x"}})
			if err == nil || !strings.Contains(err.Error(), tc.want) { t.Fatalf("error=%v want contains %q", err, tc.want) }
		})
	}
}

func TestBoolIntJSONForms(t *testing.T) {
	for _, raw := range []string{`true`, `1`} {
		var v BoolInt
		if err := v.UnmarshalJSON([]byte(raw)); err != nil || !bool(v) { t.Fatalf("true form %s: %v %v", raw, v, err) }
	}
	for _, raw := range []string{`false`, `0`} {
		var v BoolInt
		if err := v.UnmarshalJSON([]byte(raw)); err != nil || bool(v) { t.Fatalf("false form %s: %v %v", raw, v, err) }
	}
	var v BoolInt
	if err := v.UnmarshalJSON([]byte(`"bad"`)); err == nil { t.Fatal("bad BoolInt accepted") }
}
