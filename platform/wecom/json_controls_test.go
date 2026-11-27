package wecom

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"github.com/gorilla/websocket"
)

// deviceLogPasteSample is the user paste that produced no bot reply.
// The receive line's ASCII column contains raw ETX (0x03) and EOT (0x04),
// the non-printable bytes of the hex frame "34 03 04 CD CC 80 41 C0 53".
func deviceLogPasteSample() string {
	return "AVVOR_PM2d5_A670 驱动，用网口连接，数据采集不对 ，仪器上16.1微克每立方米【发送】 13:57:32.691 <34 03 00 1C 00 02 00 68 | 4{3}{0}{28}{0}{2}{0}h>\n" +
		"【接收】 13:57:33.579 <34 03 04 CD CC 80 41 C0 53 | 4\x03\x04吞€A繱>\n" +
		"【解析】 13:57:33.582 截取十六进制指定字节 <SubHex> <3,4>：<340304CDCC8041C053> = <CDCC8041>\n" +
		"【解析】 13:57:33.585 解浮点数 <Hex4BToDec><reverse=False>：<CDCC8041> = <-428869700>\n"
}

func TestEscapeRawJSONStringControls(t *testing.T) {
	t.Parallel()

	pretty := []byte("{\n  \"s\": \"<SubHex> ok\"\n}\n")
	if got := escapeRawJSONStringControls(pretty); !bytes.Equal(got, pretty) {
		t.Fatalf("valid JSON was rewritten:\n got %q\nwant %q", got, pretty)
	}

	rawNewline := []byte("{\"s\":\"line1\nline2\"}")
	relaxed := escapeRawJSONStringControls(rawNewline)
	if bytes.Equal(relaxed, rawNewline) {
		t.Fatal("raw newline inside a string was left in place")
	}
	var decoded struct {
		S string `json:"s"`
	}
	if err := json.Unmarshal(relaxed, &decoded); err != nil {
		t.Fatalf("relaxed newline: %v", err)
	}
	if decoded.S != "line1\nline2" {
		t.Fatalf("decoded = %q", decoded.S)
	}

	// An escaped quote must not end the string, and an already-escaped
	// \u0003 must not be rewritten. The raw EOT after it still has to be.
	mixed := []byte("{\"s\":\"say \\\"hi\\\" \\u0003raw\x04\"}")
	relaxed = escapeRawJSONStringControls(mixed)
	if err := json.Unmarshal(relaxed, &decoded); err != nil {
		t.Fatalf("relaxed mixed escapes: %v\njson: %s", err, relaxed)
	}
	if decoded.S != "say \"hi\" \x03raw\x04" {
		t.Fatalf("decoded = %q", decoded.S)
	}

	structural := []byte("{\n\"s\": \"a\x03b\"\n}")
	relaxed = escapeRawJSONStringControls(structural)
	if !bytes.Contains(relaxed, []byte("{\n\"s\"")) {
		t.Fatalf("structural newline was escaped: %q", relaxed)
	}
	if err := json.Unmarshal(relaxed, &decoded); err != nil {
		t.Fatalf("relaxed structural: %v", err)
	}
	if decoded.S != "a\x03b" {
		t.Fatalf("decoded = %q", decoded.S)
	}
}

func TestUnmarshalWSFrame_InvalidJSONStillFails(t *testing.T) {
	t.Parallel()
	if _, err := unmarshalWSFrame([]byte(`{"cmd":`)); err == nil {
		t.Fatal("truncated JSON parsed")
	}
	if _, err := unmarshalWSFrame([]byte("{\"cmd\":\"aibot_msg_callback\",\"headers\":{\"req_id\":\"x\"},}")); err == nil {
		t.Fatal("trailing comma parsed")
	}
}

func TestUnmarshalWSFrame_QuoteKeepsControlsAndTags(t *testing.T) {
	t.Parallel()

	raw := rawTextCallbackFrame("req-quote", "msg-quote", "grp", "u1", "see <SubHex>\x03", "old <Hex4BToDec>\x04")
	if err := json.Unmarshal(raw, new(wsFrame)); err == nil {
		t.Fatal("strict JSON parser accepted raw controls; the drop is no longer reproduced")
	}
	frame, err := unmarshalWSFrame(raw)
	if err != nil {
		t.Fatalf("unmarshalWSFrame: %v", err)
	}

	p, captured := newCapturedWSPlatform()
	p.botID = "Robot01"
	p.handleMsgCallback(frame)

	select {
	case msg := <-captured:
		if msg.Content != "see <SubHex>\x03" {
			t.Fatalf("Content = %q", msg.Content)
		}
		wantQuote := "[Quoted message]:\nold <Hex4BToDec>\x04\n\n"
		if msg.ExtraContent != wantQuote {
			t.Fatalf("ExtraContent = %q, want %q", msg.ExtraContent, wantQuote)
		}
	case <-time.After(time.Second):
		t.Fatal("handler not called")
	}
}

func TestRunConnection_DeviceLogPasteReachesHandler(t *testing.T) {
	sample := deviceLogPasteSample()
	rawLog := rawTextCallbackFrame("req-log", "msg-log", "grp", "u1", sample, "")
	if err := json.Unmarshal(rawLog, new(wsFrame)); err == nil {
		t.Fatal("strict JSON parser accepted the device-log frame; the drop is no longer reproduced")
	}

	command := rawTextCallbackFrame("req-cmd", "msg-cmd", "grp", "u1", "@Robot01 /status", "")
	if err := json.Unmarshal(command, new(wsFrame)); err != nil {
		t.Fatalf("slash command frame should already be valid JSON: %v", err)
	}
	tagged := rawTextCallbackFrame("req-tag", "msg-tag", "grp", "u1", "<Hex4BToDec><reverse=False> **标题：**内容", "")
	if err := json.Unmarshal(tagged, new(wsFrame)); err != nil {
		t.Fatalf("angle-bracket frame should already be valid JSON: %v", err)
	}

	received := make(chan *core.Message, 4)
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("read subscribe: %v", err)
			return
		}
		ack := []byte(`{"headers":{"req_id":"aibot_subscribe_1"},"errcode":0,"errmsg":"ok"}`)
		if err := conn.WriteMessage(websocket.TextMessage, ack); err != nil {
			t.Errorf("write subscribe ack: %v", err)
			return
		}
		for _, frame := range [][]byte{rawLog, command, tagged} {
			if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
				t.Errorf("write frame: %v", err)
				return
			}
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))

	ctx, cancel := context.WithCancel(context.Background())
	p := &WSPlatform{
		botID:     "Robot01",
		secret:    "secret",
		allowFrom: "*",
		wsURL:     "ws" + strings.TrimPrefix(srv.URL, "http"),
		ctx:       ctx,
		cancel:    cancel,
		handler: func(_ core.Platform, msg *core.Message) {
			received <- msg
		},
	}
	t.Cleanup(func() {
		_ = p.Stop()
		srv.Close()
	})

	go func() { _ = p.runConnection() }()

	var got []*core.Message
	deadline := time.After(3 * time.Second)
	for len(got) < 3 {
		select {
		case msg := <-received:
			got = append(got, msg)
		case <-deadline:
			t.Fatalf("handler received %d messages, want 3 (device-log paste was dropped)", len(got))
		}
	}

	// emitInbound runs in its own goroutine, so arrival order is not the send order.
	var logText, commandText, taggedText string
	for _, msg := range got {
		switch {
		case strings.Contains(msg.Content, "AVVOR_PM2d5_A670"):
			logText = msg.Content
		case msg.Content == "/status":
			commandText = msg.Content
		default:
			taggedText = msg.Content
		}
	}
	if logText != strings.TrimSpace(sample) {
		t.Fatalf("device log content mismatch\n got %q\nwant %q", logText, strings.TrimSpace(sample))
	}
	for _, piece := range []string{"<SubHex>", "<3,4>", "<Hex4BToDec>", "<reverse=False>", "数据采集不对", "\x03", "\x04", "吞€A繱"} {
		if !strings.Contains(logText, piece) {
			t.Fatalf("delivered text missing %q\ncontent: %q", piece, logText)
		}
	}
	if commandText != "/status" {
		t.Fatalf("command content = %q, want /status", commandText)
	}
	if taggedText != "<Hex4BToDec><reverse=False> **标题：**内容" {
		t.Fatalf("tagged markdown content = %q", taggedText)
	}
}

// rawTextCallbackFrame builds an aibot_msg_callback frame. content and quote are
// spliced into JSON strings with only quotes and backslashes escaped, so raw
// newlines and other control bytes stay raw — the shape WeCom actually sends.
// quote is omitted when empty.
func rawTextCallbackFrame(reqID, msgID, chatID, userID, content, quote string) []byte {
	var b bytes.Buffer
	b.WriteString(`{"cmd":"aibot_msg_callback","headers":{"req_id":`)
	b.WriteString(strconv.Quote(reqID))
	b.WriteString(`},"body":{"msgid":`)
	b.WriteString(strconv.Quote(msgID))
	b.WriteString(`,"aibotid":"Robot01","chatid":`)
	b.WriteString(strconv.Quote(chatID))
	b.WriteString(`,"chattype":"group","from":{"userid":`)
	b.WriteString(strconv.Quote(userID))
	b.WriteString(`},"msgtype":"text","text":{"content":`)
	appendRawJSONString(&b, content)
	b.WriteByte('}')
	if quote != "" {
		b.WriteString(`,"quote":{"msgtype":"text","text":{"content":`)
		appendRawJSONString(&b, quote)
		b.WriteString(`}}`)
	}
	b.WriteString(`,"create_time":`)
	b.WriteString(strconv.FormatInt(time.Now().Unix(), 10))
	b.WriteString(`}}`)
	return b.Bytes()
}

func appendRawJSONString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\', '"':
			b.WriteByte('\\')
			b.WriteByte(s[i])
		default:
			b.WriteByte(s[i])
		}
	}
	b.WriteByte('"')
}
