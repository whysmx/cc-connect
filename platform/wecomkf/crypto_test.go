package wecomkf

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"testing"
)

// testAESKeyEncoded is a 43-char EncodingAESKey (32 bytes of 0x11).
var testAESKeyEncoded = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x11}, 32))[:43]

// encryptForTest mirrors WeCom's WXBizMsgCrypt encryption.
func encryptForTest(t *testing.T, key []byte, msg, receiveID string) string {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	body := append(bytes.Repeat([]byte{'r'}, 16), make([]byte, 4)...)
	binary.BigEndian.PutUint32(body[16:20], uint32(len(msg)))
	body = append(body, msg...)
	body = append(body, receiveID...)
	pad := 32 - len(body)%32
	body = append(body, bytes.Repeat([]byte{byte(pad)}, pad)...)
	out := make([]byte, len(body))
	cipher.NewCBCEncrypter(block, key[:16]).CryptBlocks(out, body)
	return base64.StdEncoding.EncodeToString(out)
}

func TestVerifySignature(t *testing.T) {
	const want = "8c6f84ef413f28c2cc9d769d230086a1a493b47c"
	if got := computeSignature("token", "1700000000", "nonce", "cipher"); got != want {
		t.Fatalf("computeSignature = %s, want %s", got, want)
	}
	if !verifySignature("token", "1700000000", "nonce", "cipher", "8C6F84EF413F28C2CC9D769D230086A1A493B47C") {
		t.Fatal("valid (upper-case) signature rejected")
	}
	for _, bad := range []string{"", "bad", want[:39] + "0"} {
		if verifySignature("token", "1700000000", "nonce", "cipher", bad) {
			t.Fatalf("invalid signature %q accepted", bad)
		}
	}
}

func TestDecodeAESKey(t *testing.T) {
	key, err := decodeAESKey(testAESKeyEncoded)
	if err != nil {
		t.Fatalf("decodeAESKey: %v", err)
	}
	if !bytes.Equal(key, bytes.Repeat([]byte{0x11}, 32)) {
		t.Fatal("decoded key mismatch")
	}
	for _, bad := range []string{"", "short", testAESKeyEncoded + "A", "!!!" + testAESKeyEncoded[3:]} {
		if _, err := decodeAESKey(bad); err == nil {
			t.Fatalf("decodeAESKey(%q) accepted invalid key", bad)
		}
	}
}

func TestDecryptPayload_RoundTripAndReceiveID(t *testing.T) {
	key, _ := decodeAESKey(testAESKeyEncoded)
	msg := "<xml><Event>kf_msg_or_event</Event><Token>tok</Token><OpenKfId>wk1</OpenKfId></xml>"
	enc := encryptForTest(t, key, msg, "corp1")

	plain, err := decryptPayload(key, enc, "corp1")
	if err != nil {
		t.Fatalf("decryptPayload: %v", err)
	}
	if string(plain) != msg {
		t.Fatalf("plain = %q", plain)
	}
	if _, err := decryptPayload(key, enc, "other"); err != errReceiveIDMismatch {
		t.Fatalf("receive ID mismatch err = %v", err)
	}
	if _, err := decryptPayload(key, "not base64!", ""); err != errInvalidCiphertext {
		t.Fatalf("bad base64 err = %v", err)
	}
	otherKey := bytes.Repeat([]byte{0x22}, 32)
	if _, err := decryptPayload(otherKey, enc, "corp1"); err == nil {
		t.Fatal("decrypt with wrong key succeeded")
	}
}

func TestParseCallbackEvent(t *testing.T) {
	ev, err := parseCallbackEvent([]byte("<xml><ToUserName><![CDATA[corp]]></ToUserName><CreateTime>1</CreateTime><MsgType><![CDATA[event]]></MsgType><Event><![CDATA[kf_msg_or_event]]></Event><Token><![CDATA[T]]></Token><OpenKfId><![CDATA[wk]]></OpenKfId></xml>"))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Event != eventKfMsgOrEvent || ev.Token != "T" || ev.OpenKfID != "wk" || ev.ToUserName != "corp" {
		t.Fatalf("unexpected event %+v", ev)
	}
	if _, err := parseCallbackEvent([]byte("<xml")); err == nil {
		t.Fatal("malformed XML accepted")
	}
}
