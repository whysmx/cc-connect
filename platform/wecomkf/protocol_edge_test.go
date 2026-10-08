package wecomkf

import (
	"bytes"
	"encoding/base64"
	"errors"
	"testing"
)

func TestProtocolValidationEdges(t *testing.T) {
	if _, err := DecodeAESKey(""); err == nil { t.Fatal("empty key accepted") }
	if _, err := DecodeAESKey("bad"); err == nil { t.Fatal("bad key accepted") }
	if _, err := DecodeAESKey(base64.StdEncoding.EncodeToString([]byte("short"))); err == nil { t.Fatal("short key accepted") }
	if _, err := Decrypt("bad", bytes.Repeat([]byte{1}, 32), ""); !errors.Is(err, ErrInvalidCiphertext) { t.Fatalf("bad ciphertext error = %v", err) }
	if _, err := Decrypt(base64.StdEncoding.EncodeToString([]byte("short")), bytes.Repeat([]byte{1}, 32), ""); !errors.Is(err, ErrInvalidCiphertext) { t.Fatalf("short ciphertext error = %v", err) }
	if _, err := ParseEvent([]byte("<xml>")); err == nil { t.Fatal("malformed event accepted") }
	if _, err := ParseEvent([]byte("<xml><Event>other</Event><Token>x</Token><OpenKfId>wk</OpenKfId></xml>")); err == nil { t.Fatal("wrong event accepted") }
	if _, err := ParseEvent([]byte("<xml><Event>kf_msg_or_event</Event></xml>")); err == nil { t.Fatal("incomplete event accepted") }
	if VerifySignature("a", "b", "c", "d", "not-a-signature") { t.Fatal("bad signature accepted") }
	if got := SplitTextByBytes("", 10); got != nil { t.Fatalf("empty split = %#v", got) }
	if got := SplitTextByBytes("abc", 0); got != nil { t.Fatalf("zero-limit split = %#v", got) }
	if got := SplitTextByBytes("abc", 10); len(got) != 1 || got[0] != "abc" { t.Fatalf("large split = %#v", got) }
}

func TestProtocolInvalidPaddingAndLength(t *testing.T) {
	key := bytes.Repeat([]byte{0x11}, 32)
	valid := encryptForProtocolTest(t, []byte("hello"), "corp", key)
	raw, err := base64.StdEncoding.DecodeString(valid)
	if err != nil { t.Fatal(err) }
	raw[len(raw)-1] ^= 1
	if _, err := Decrypt(base64.StdEncoding.EncodeToString(raw), key, "corp"); !errors.Is(err, ErrInvalidCiphertext) { t.Fatalf("bad padding error = %v", err) }
}

func encryptForProtocolTest(t *testing.T, message []byte, receiveID string, key []byte) string {
	t.Helper()
	body := append(bytes.Repeat([]byte{0x66}, 16), make([]byte, 4)...)
	binary.BigEndian.PutUint32(body[16:20], uint32(len(message)))
	body = append(body, message...)
	body = append(body, receiveID...)
	pad := 32 - len(body)%32
	body = append(body, bytes.Repeat([]byte{byte(pad)}, pad)...)
	block, err := aes.NewCipher(key)
	if err != nil { t.Fatal(err) }
	out := make([]byte, len(body))
	cipher.NewCBCEncrypter(block, key[:aes.BlockSize]).CryptBlocks(out, body)
	return base64.StdEncoding.EncodeToString(out)
}
