package wecomkf

import (
    "bytes"
    "context"
    "crypto/aes"
    "crypto/cipher"
    "encoding/base64"
    "encoding/json"
    "errors"
    "io"
    "net/http"
    "strings"
    "testing"
)

type marshalError struct{}
func (marshalError) MarshalJSON() ([]byte, error) { return nil, errors.New("marshal boom") }

type roundTripFunc func(*http.Request) (*http.Response, error)
func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type errorReader struct{}
func (errorReader) Read([]byte) (int, error) { return 0, errors.New("read boom") }
func (errorReader) Close() error { return nil }

func TestPostJSONTransportAndEncodingErrors(t *testing.T) {
    c := NewClient("http://example.test", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
        return nil, errors.New("transport boom")
    })})
    if err := c.postJSON(context.Background(), "/x", "token", marshalError{}, &struct{}{}); err == nil || !strings.Contains(err.Error(), "encode /x") { t.Fatalf("marshal error = %v", err) }
    badURL := NewClient("://bad", http.DefaultClient)
    if err := badURL.postJSON(context.Background(), "/x", "token", struct{}{}, &struct{}{}); err == nil || !strings.Contains(err.Error(), "build /x URL") { t.Fatalf("url error = %v", err) }
    if err := c.postJSON(context.Background(), "/x", "token", struct{}{}, &struct{}{}); err == nil || !strings.Contains(err.Error(), "request /x") { t.Fatalf("transport error = %v", err) }
}

func TestPostJSONReadAndRequestContextErrors(t *testing.T) {
    c := NewClient("http://example.test", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
        return &http.Response{StatusCode: http.StatusOK, Body: errorReader{}}, nil
    })})
    if err := c.postJSON(context.Background(), "/read", "token", struct{}{}, &struct{}{}); err == nil || !strings.Contains(err.Error(), "read /read response") { t.Fatalf("read error = %v", err) }
    canceled, cancel := context.WithCancel(context.Background()); cancel()
    c = NewClient("http://example.test", &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
        if err := r.Context().Err(); err != nil { return nil, err }
        return nil, errors.New("unexpected")
    })})
    if err := c.postJSON(canceled, "/ctx", "token", struct{}{}, &struct{}{}); err == nil || !strings.Contains(err.Error(), "request /ctx") { t.Fatalf("context error = %v", err) }
}

func TestSyncMessagesNormalizesLimit(t *testing.T) {
    var limits []int
    c := NewClient("http://example.test", &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
        var req SyncMessageRequest
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil { return nil, err }
        limits = append(limits, req.Limit)
        return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"errcode":0}`))}, nil
    })})
    for _, limit := range []int{0, -1, 1001, 25} {
        if _, err := c.SyncMessages(context.Background(), "token", SyncMessageRequest{Limit: limit}); err != nil { t.Fatal(err) }
    }
    want := []int{1000, 1000, 1000, 25}
    for i := range want { if limits[i] != want[i] { t.Fatalf("limits = %v, want %v", limits, want) } }
}

func TestBoolIntAdditionalInvalidJSON(t *testing.T) {
    for _, raw := range []string{`null`, `1.5`, `{}`} {
        var v BoolInt
        if err := v.UnmarshalJSON([]byte(raw)); err == nil { t.Fatalf("invalid BoolInt %s accepted", raw) }
    }
}

func TestDecryptMalformedPlaintextAndLength(t *testing.T) {
    key := bytes.Repeat([]byte{0x44}, 32)
    // A decryptable block that unpads to fewer than the 20-byte header.
    short := encryptRawForCoverage(t, make([]byte, 16), key)
    if _, err := Decrypt(short, key, ""); !errors.Is(err, ErrInvalidCiphertext) { t.Fatalf("short plaintext error = %v", err) }
    // Header advertises more message bytes than remain after it.
    malformed := make([]byte, 20)
    malformed[16], malformed[17], malformed[18], malformed[19] = 0, 0, 0, 100
    malformed = append(malformed, bytes.Repeat([]byte{16}, 16)...)
    if _, err := Decrypt(encryptRawForCoverage(t, malformed, key), key, ""); !errors.Is(err, ErrInvalidCiphertext) { t.Fatalf("length error = %v", err) }
}

func encryptRawForCoverage(t *testing.T, plain []byte, key []byte) string {
    t.Helper()
    pad := 32 - len(plain)%32
    plain = append(append([]byte(nil), plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
    block, err := aes.NewCipher(key)
    if err != nil { t.Fatal(err) }
    out := make([]byte, len(plain))
    cipher.NewCBCEncrypter(block, key[:aes.BlockSize]).CryptBlocks(out, plain)
    return base64.StdEncoding.EncodeToString(out)
}

func TestPKCS7PaddingValidationEdges(t *testing.T) {
    cases := [][]byte{
        nil,
        []byte{1, 2}, // not a complete block
        bytes.Repeat([]byte{0}, 32), // zero pad
        append(bytes.Repeat([]byte{0}, 31), 32), // pad exceeds data
        append(bytes.Repeat([]byte{0}, 30), 2, 3), // mismatched pad bytes
    }
    for _, data := range cases {
        if _, err := unpadPKCS7(data, 32); !errors.Is(err, ErrInvalidCiphertext) { t.Fatalf("unpad(%v) error = %v", data, err) }
    }
    got, err := unpadPKCS7(append(bytes.Repeat([]byte{'x'}, 30), 2, 2), 32)
    if err != nil || string(got) != strings.Repeat("x", 30) { t.Fatalf("valid unpad = %q, %v", got, err) }
}


func TestDecodeAESKeyAndDecryptKeyLengthErrors(t *testing.T) {
    if _, err := DecodeAESKey("not-base64!!!"); err == nil || !strings.Contains(err.Error(), "decode encoding AES key") { t.Fatalf("invalid base64 error = %v", err) }
    if _, err := Decrypt("", bytes.Repeat([]byte{1}, 31), ""); err == nil || !strings.Contains(err.Error(), "AES key length") { t.Fatalf("short AES key error = %v", err) }
}
