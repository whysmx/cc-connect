package wecomkf

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientSyncAndSend(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("access_token") != "token" {
			t.Fatalf("access token = %q", r.URL.Query().Get("access_token"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/cgi-bin/kf/sync_msg" {
			_, _ = w.Write([]byte(`{"errcode":0,"next_cursor":"c2","has_more":true,"msg_list":[{"msgid":"m1","open_kfid":"wk1","external_userid":"u1","origin":3,"msgtype":"text","text":{"content":"hello"}}]}`))
			return
		}
		if body["msgtype"] != "text" {
			t.Fatalf("msgtype = %#v", body["msgtype"])
		}
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, server.Client())
	got, err := client.SyncMessages(context.Background(), "token", SyncMessageRequest{Token: "pull", OpenKfID: "wk1"})
	if err != nil {
		t.Fatalf("SyncMessages() error = %v", err)
	}
	if got.NextCursor != "c2" || !got.HasMore || len(got.MsgList) != 1 {
		t.Fatalf("unexpected response: %+v", got)
	}
	if err := client.SendText(context.Background(), "token", SendTextRequest{ToUser: "u1", OpenKfID: "wk1", Text: MessageText{Content: "answer"}}); err != nil {
		t.Fatalf("SendText() error = %v", err)
	}
}

func TestClientRejectsAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errcode":95002,"errmsg":"expired"}`))
	}))
	defer server.Close()
	err := NewClient(server.URL, server.Client()).SendText(context.Background(), "token", SendTextRequest{ToUser: "u1", OpenKfID: "wk1", Text: MessageText{Content: "answer"}})
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.Code != 95002 {
		t.Fatalf("error = %#v", err)
	}
}
