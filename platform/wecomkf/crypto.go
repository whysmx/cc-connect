package wecomkf

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Errors returned by the callback crypto helpers.
var (
	errInvalidCiphertext = errors.New("wecom_kf: invalid callback ciphertext")
	errReceiveIDMismatch = errors.New("wecom_kf: callback receive ID mismatch")
)

// callbackEnvelope is the outer (still encrypted) XML body POSTed by WeCom.
type callbackEnvelope struct {
	XMLName    xml.Name `xml:"xml"`
	ToUserName string   `xml:"ToUserName"`
	AgentID    string   `xml:"AgentID"`
	Encrypt    string   `xml:"Encrypt"`
}

// callbackEvent is the decrypted WeChat Customer Service notification.
// It only announces that new messages exist; the content must be pulled with
// kf/sync_msg using Token + OpenKfID.
type callbackEvent struct {
	XMLName    xml.Name `xml:"xml"`
	ToUserName string   `xml:"ToUserName"`
	CreateTime int64    `xml:"CreateTime"`
	MsgType    string   `xml:"MsgType"`
	Event      string   `xml:"Event"`
	Token      string   `xml:"Token"`
	OpenKfID   string   `xml:"OpenKfId"`
}

const eventKfMsgOrEvent = "kf_msg_or_event"

// computeSignature returns the WeCom msg_signature: sha1 over the sorted
// concatenation of token, timestamp, nonce and the encrypted payload.
func computeSignature(token, timestamp, nonce, encrypted string) string {
	parts := []string{token, timestamp, nonce, encrypted}
	sort.Strings(parts)
	sum := sha1.Sum([]byte(strings.Join(parts, "")))
	return hex.EncodeToString(sum[:])
}

// verifySignature checks msg_signature in constant time.
func verifySignature(token, timestamp, nonce, encrypted, signature string) bool {
	if signature == "" {
		return false
	}
	want := computeSignature(token, timestamp, nonce, encrypted)
	got := strings.ToLower(strings.TrimSpace(signature))
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// decodeAESKey decodes the 43-character EncodingAESKey configured in WeCom.
func decodeAESKey(encoded string) ([]byte, error) {
	encoded = strings.TrimSpace(encoded)
	if len(encoded) != 43 {
		return nil, fmt.Errorf("wecom_kf: callback_aes_key must be 43 characters, got %d", len(encoded))
	}
	key, err := base64.StdEncoding.DecodeString(encoded + "=")
	if err != nil {
		return nil, fmt.Errorf("wecom_kf: decode callback_aes_key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("wecom_kf: callback_aes_key decodes to %d bytes, want 32", len(key))
	}
	return key, nil
}

// decryptPayload decrypts a WeCom callback payload (AES-256-CBC, IV = key[:16],
// PKCS#7 padding to 32 bytes). The plaintext layout is
// random(16) | msg_len(4, big endian) | msg | receive_id.
// When expectedReceiveID is non-empty it must match the trailing receive ID
// (the CorpID for WeChat Customer Service callbacks).
func decryptPayload(aesKey []byte, encrypted, expectedReceiveID string) ([]byte, error) {
	if len(aesKey) != 32 {
		return nil, fmt.Errorf("wecom_kf: AES key length %d, want 32", len(aesKey))
	}
	ciphertext, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encrypted))
	if err != nil || len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return nil, errInvalidCiphertext
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, fmt.Errorf("wecom_kf: create cipher: %w", err)
	}
	plain := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, aesKey[:aes.BlockSize]).CryptBlocks(plain, ciphertext)

	plain, err = pkcs7Unpad(plain)
	if err != nil || len(plain) < 20 {
		return nil, errInvalidCiphertext
	}
	msgLen := int(binary.BigEndian.Uint32(plain[16:20]))
	if msgLen < 0 || 20+msgLen > len(plain) {
		return nil, errInvalidCiphertext
	}
	msg := plain[20 : 20+msgLen]
	receiveID := string(plain[20+msgLen:])
	if expectedReceiveID != "" && receiveID != expectedReceiveID {
		return nil, errReceiveIDMismatch
	}
	return msg, nil
}

// pkcs7Unpad strips WeCom's PKCS#7 padding (block size 32).
func pkcs7Unpad(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, errInvalidCiphertext
	}
	pad := int(data[len(data)-1])
	if pad < 1 || pad > 32 || pad > len(data) {
		return nil, errInvalidCiphertext
	}
	for _, b := range data[len(data)-pad:] {
		if int(b) != pad {
			return nil, errInvalidCiphertext
		}
	}
	return data[:len(data)-pad], nil
}

// parseCallbackEvent parses a decrypted callback body.
func parseCallbackEvent(plain []byte) (callbackEvent, error) {
	var ev callbackEvent
	if err := xml.Unmarshal(plain, &ev); err != nil {
		return callbackEvent{}, fmt.Errorf("wecom_kf: parse callback event: %w", err)
	}
	return ev, nil
}
