package wecomkf

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"testing"
)

func TestVerifySignature(t *testing.T) {
	if !VerifySignature("token", "1700000000", "nonce", "cipher", "8c6f84ef413f28c2cc9d769d230086a1a493b47c") {
		t.Fatal("valid signature rejected")
	}
	if VerifySignature("token", "1700000000", "nonce", "cipher", "bad") {
		t.Fatal("invalid signature accepted")
	}
}

func TestDecryptAndParseEvent(t *testing.T) {
	key := bytes.Repeat([]byte{0x11}, 32)
	message := []byte("<xml><ToUserName>corp</ToUserName><CreateTime>1700000000</CreateTime><MsgType>event</MsgType><Event>kf_msg_or_event</Event><Token>pull-token</Token><OpenKfId>wk123</OpenKfId></xml>")
	encrypted := encryptCallbackForTest(t, message, "corp", key)

	plain, err := Decrypt(encrypted, key, "corp")
	if err != nil {
		t.Fatalf("Decrypt() error = %v", err)
	}
	event, err := ParseEvent([]byte(plain))
	if err != nil {
		t.Fatalf("ParseEvent() error = %v", err)
	}
	if event.Token != "pull-token" || event.OpenKfID != "wk123" {
		t.Fatalf("unexpected event: %+v", event)
	}
	if _, err := Decrypt(encrypted, key, "other-corp"); err != ErrInvalidReceiveID {
		t.Fatalf("wrong receive ID error = %v, want %v", err, ErrInvalidReceiveID)
	}
}

func TestDecodeAESKey(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x22}, 32))
	encoded = encoded[:len(encoded)-1]
	got, err := DecodeAESKey(encoded)
	if err != nil {
		t.Fatalf("DecodeAESKey() error = %v", err)
	}
	if !bytes.Equal(got, bytes.Repeat([]byte{0x22}, 32)) {
		t.Fatalf("decoded key mismatch")
	}
}

func TestSessionKeyAndSplitText(t *testing.T) {
	if got := SessionKey("corp", "wk1", "user1"); got != "wecom-kf:corp:wk1:user1" {
		t.Fatalf("SessionKey() = %q", got)
	}
	chunks := SplitTextByBytes("你好世界", 7)
	if len(chunks) != 2 || chunks[0] != "你好" || chunks[1] != "世界" {
		t.Fatalf("unexpected chunks: %#v", chunks)
	}
}

func encryptCallbackForTest(t *testing.T, message []byte, receiveID string, key []byte) string {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	body := append(bytes.Repeat([]byte{0x33}, 16), make([]byte, 4)...)
	binary.BigEndian.PutUint32(body[16:20], uint32(len(message)))
	body = append(body, message...)
	body = append(body, receiveID...)
	pad := 32 - len(body)%32
	body = append(body, bytes.Repeat([]byte{byte(pad)}, pad)...)
	ciphertext := make([]byte, len(body))
	cipher.NewCBCEncrypter(block, key[:aes.BlockSize]).CryptBlocks(ciphertext, body)
	return base64.StdEncoding.EncodeToString(ciphertext)
}
