package wecomkf

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"sort"
	"strings"
)

var (
	ErrInvalidSignature = errors.New("wecomkf: invalid callback signature")
	ErrInvalidCiphertext = errors.New("wecomkf: invalid callback ciphertext")
	ErrInvalidReceiveID = errors.New("wecomkf: callback receive ID mismatch")
)

// CustomerServiceEvent is the decrypted notification sent by WeCom Customer Service.
// The notification only announces that messages are available; callers must use
// the Token and OpenKfId to pull them with kf/sync_msg.
type CustomerServiceEvent struct {
	XMLName    xml.Name `xml:"xml"`
	ToUserName string   `xml:"ToUserName"`
	CreateTime int64    `xml:"CreateTime"`
	MsgType    string   `xml:"MsgType"`
	Event      string   `xml:"Event"`
	Token      string   `xml:"Token"`
	OpenKfID   string   `xml:"OpenKfId"`
}

// VerifySignature verifies msg_signature using the WeCom SHA-1 rule.
func VerifySignature(token, timestamp, nonce, encrypted, signature string) bool {
	values := []string{token, timestamp, nonce, encrypted}
	sort.Strings(values)
	sum := sha1.Sum([]byte(strings.Join(values, "")))
	return strings.EqualFold(fmt.Sprintf("%x", sum[:]), strings.TrimSpace(signature))
}

// DecodeAESKey decodes the 43-character EncodingAESKey used by WeCom.
func DecodeAESKey(encoded string) ([]byte, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return nil, fmt.Errorf("wecomkf: empty encoding AES key")
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded + "=")
	if err != nil {
		return nil, fmt.Errorf("wecomkf: decode encoding AES key: %w", err)
	}
	if len(decoded) != 32 {
		return nil, fmt.Errorf("wecomkf: encoding AES key decoded length is %d, want 32", len(decoded))
	}
	return decoded, nil
}

// Decrypt decrypts a WeCom callback payload and validates its receive ID when
// expectedReceiveID is non-empty.
func Decrypt(encrypted string, aesKey []byte, expectedReceiveID string) (string, error) {
	if len(aesKey) != 32 {
		return "", fmt.Errorf("wecomkf: AES key length is %d, want 32", len(aesKey))
	}
	ciphertext, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encrypted))
	if err != nil || len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return "", ErrInvalidCiphertext
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return "", fmt.Errorf("wecomkf: create AES cipher: %w", err)
	}
	plain := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, aesKey[:aes.BlockSize]).CryptBlocks(plain, ciphertext)
	plain, err = unpadPKCS7(plain, 32)
	if err != nil || len(plain) < 20 {
		return "", ErrInvalidCiphertext
	}
	msgLen := binary.BigEndian.Uint32(plain[16:20])
	if uint64(msgLen) > uint64(len(plain)-20) {
		return "", ErrInvalidCiphertext
	}
	msgEnd := 20 + int(msgLen)
	message := plain[20:msgEnd]
	receiveID := string(plain[msgEnd:])
	if expectedReceiveID != "" && receiveID != expectedReceiveID {
		return "", ErrInvalidReceiveID
	}
	return string(message), nil
}

// ParseEvent parses a decrypted kf_msg_or_event notification.
func ParseEvent(payload []byte) (CustomerServiceEvent, error) {
	var event CustomerServiceEvent
	if err := xml.Unmarshal(payload, &event); err != nil {
		return CustomerServiceEvent{}, fmt.Errorf("wecomkf: parse callback event: %w", err)
	}
	if event.Event != "kf_msg_or_event" {
		return CustomerServiceEvent{}, fmt.Errorf("wecomkf: unsupported event %q", event.Event)
	}
	if event.Token == "" || event.OpenKfID == "" {
		return CustomerServiceEvent{}, fmt.Errorf("wecomkf: callback event is missing Token or OpenKfId")
	}
	return event, nil
}

// SessionKey returns a collision-resistant logical session key.
func SessionKey(corpID, openKfID, externalUserID string) string {
	return "wecom-kf:" + corpID + ":" + openKfID + ":" + externalUserID
}

// SplitTextByBytes splits UTF-8 text without cutting a rune.
func SplitTextByBytes(text string, maxBytes int) []string {
	if text == "" || maxBytes <= 0 {
		return nil
	}
	var chunks []string
	for len(text) > maxBytes {
		cut := maxBytes
		for cut > 0 && (text[cut]&0xc0) == 0x80 {
			cut--
		}
		if cut == 0 {
			cut = maxBytes
		}
		chunks = append(chunks, text[:cut])
		text = text[cut:]
	}
	if text != "" {
		chunks = append(chunks, text)
	}
	return chunks
}

func unpadPKCS7(data []byte, blockSize int) ([]byte, error) {
	if len(data) == 0 || len(data)%blockSize != 0 {
		return nil, ErrInvalidCiphertext
	}
	pad := int(data[len(data)-1])
	if pad == 0 || pad > blockSize || pad > len(data) {
		return nil, ErrInvalidCiphertext
	}
	for _, b := range data[len(data)-pad:] {
		if int(b) != pad {
			return nil, ErrInvalidCiphertext
		}
	}
	return data[:len(data)-pad], nil
}
