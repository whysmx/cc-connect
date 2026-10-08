package wecomkf

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

const defaultAPIBaseURL = "https://qyapi.weixin.qq.com"

// Message origins returned by kf/sync_msg.
const (
	originCustomer = 3 // sent by the WeChat customer
	originSystem   = 4 // system event
	originServicer = 5 // sent by a human servicer from the WeCom client
)

// Session states returned by kf/service_state/get.
const (
	stateUnhandled  = 0 // new session, may be answered via API
	stateAssistant  = 1 // handled by the API / smart assistant
	stateQueued     = 2 // waiting in the human servicer pool
	stateHuman      = 3 // handled by a human servicer
	stateEndedOrNew = 4 // ended / not started
)

// apiError is a non-zero errcode returned by the WeCom API.
type apiError struct {
	Op      string
	ErrCode int
	ErrMsg  string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("wecom_kf: %s: errcode=%d errmsg=%s", e.Op, e.ErrCode, e.ErrMsg)
}

// isTokenError reports access_token problems that a token refresh can fix.
func isTokenError(code int) bool {
	switch code {
	case 40001, 40014, 42001:
		return true
	}
	return false
}

// isRetryable reports whether err is worth retrying (network failure or a
// transient "system busy" from WeCom).
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.ErrCode == -1 || isTokenError(ae.ErrCode)
	}
	return !errors.Is(err, context.Canceled)
}

type kfText struct {
	Content string `json:"content"`
	MenuID  string `json:"menu_id,omitempty"`
}

type kfEvent struct {
	EventType      string `json:"event_type"`
	OpenKfID       string `json:"open_kfid"`
	ExternalUserID string `json:"external_userid"`
	ChangeType     int    `json:"change_type"`
	OldServicer    string `json:"old_servicer_userid"`
	NewServicer    string `json:"new_servicer_userid"`
	FailMsgID      string `json:"fail_msgid"`
	FailType       int    `json:"fail_type"`
}

// kfMessage is one entry of kf/sync_msg msg_list.
type kfMessage struct {
	MsgID          string   `json:"msgid"`
	OpenKfID       string   `json:"open_kfid"`
	ExternalUserID string   `json:"external_userid"`
	SendTime       int64    `json:"send_time"`
	Origin         int      `json:"origin"`
	ServicerUserID string   `json:"servicer_userid"`
	MsgType        string   `json:"msgtype"`
	Text           *kfText  `json:"text,omitempty"`
	Event          *kfEvent `json:"event,omitempty"`
}

type syncMsgRequest struct {
	Cursor   string `json:"cursor,omitempty"`
	Token    string `json:"token,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	OpenKfID string `json:"open_kfid"`
}

type syncMsgResponse struct {
	ErrCode    int         `json:"errcode"`
	ErrMsg     string      `json:"errmsg"`
	NextCursor string      `json:"next_cursor"`
	HasMore    int         `json:"has_more"`
	MsgList    []kfMessage `json:"msg_list"`
}

// apiClient talks to the WeChat Customer Service server API. It owns the
// access_token cache for one corp_id + secret pair.
type apiClient struct {
	baseURL    string
	corpID     string
	corpSecret string
	http       *http.Client

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

func newAPIClient(baseURL, corpID, corpSecret string, httpClient *http.Client) *apiClient {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = defaultAPIBaseURL
	}
	return &apiClient{baseURL: baseURL, corpID: corpID, corpSecret: corpSecret, http: httpClient}
}

// accessToken returns a cached access_token, refreshing it when expired or
// when force is true.
func (c *apiClient) accessToken(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && c.token != "" && time.Now().Before(c.expiresAt) {
		return c.token, nil
	}
	q := url.Values{"corpid": {c.corpID}, "corpsecret": {c.corpSecret}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/cgi-bin/gettoken?"+q.Encode(), nil)
	if err != nil {
		return "", fmt.Errorf("wecom_kf: build gettoken request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("wecom_kf: request access_token: %s", core.RedactToken(err.Error(), c.corpSecret))
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		ErrCode     int    `json:"errcode"`
		ErrMsg      string `json:"errmsg"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("wecom_kf: decode gettoken response: %w", err)
	}
	if out.ErrCode != 0 || out.AccessToken == "" {
		return "", &apiError{Op: "gettoken", ErrCode: out.ErrCode, ErrMsg: out.ErrMsg}
	}
	expires := out.ExpiresIn
	if expires <= 0 {
		expires = 7200
	}
	if expires > 120 {
		expires -= 60
	}
	c.token = out.AccessToken
	c.expiresAt = time.Now().Add(time.Duration(expires) * time.Second)
	slog.Debug("wecom_kf: access_token refreshed", "expires_in", out.ExpiresIn)
	return c.token, nil
}

func (c *apiClient) invalidateToken(stale string) {
	c.mu.Lock()
	if c.token == stale {
		c.token = ""
	}
	c.mu.Unlock()
}

// postJSON POSTs body to path and decodes the response into out. out must
// expose errcode/errmsg via the errorFields interface. A stale access_token
// is refreshed once transparently.
func (c *apiClient) postJSON(ctx context.Context, op, path string, body any, out errorFields) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("wecom_kf: %s: encode request: %w", op, err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		token, err := c.accessToken(ctx, false)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			c.baseURL+path+"?access_token="+url.QueryEscape(token), bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("wecom_kf: %s: build request: %w", op, err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("wecom_kf: %s: %s", op, core.RedactToken(err.Error(), token))
		}
		decErr := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
		_ = resp.Body.Close()
		if decErr != nil {
			return fmt.Errorf("wecom_kf: %s: decode response (http %d): %w", op, resp.StatusCode, decErr)
		}
		code, msg := out.errFields()
		if code == 0 {
			return nil
		}
		if isTokenError(code) && attempt == 0 {
			c.invalidateToken(token)
			continue
		}
		return &apiError{Op: op, ErrCode: code, ErrMsg: msg}
	}
	return &apiError{Op: op, ErrCode: -1, ErrMsg: "access_token refresh did not help"}
}

type errorFields interface {
	errFields() (int, string)
}

func (r *syncMsgResponse) errFields() (int, string) { return r.ErrCode, r.ErrMsg }

type baseResponse struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

func (r *baseResponse) errFields() (int, string) { return r.ErrCode, r.ErrMsg }

type serviceStateResponse struct {
	baseResponse
	ServiceState   int    `json:"service_state"`
	ServicerUserID string `json:"servicer_userid"`
}

type sendMsgResponse struct {
	baseResponse
	MsgID string `json:"msgid"`
}

// syncMsg pulls one page of messages for openKfID.
func (c *apiClient) syncMsg(ctx context.Context, req syncMsgRequest) (*syncMsgResponse, error) {
	var out syncMsgResponse
	if err := c.postJSON(ctx, "sync_msg", "/cgi-bin/kf/sync_msg", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// serviceState returns the current session state for a customer.
func (c *apiClient) serviceState(ctx context.Context, openKfID, externalUserID string) (int, string, error) {
	var out serviceStateResponse
	err := c.postJSON(ctx, "service_state/get", "/cgi-bin/kf/service_state/get", map[string]string{
		"open_kfid":       openKfID,
		"external_userid": externalUserID,
	}, &out)
	if err != nil {
		return 0, "", err
	}
	return out.ServiceState, out.ServicerUserID, nil
}

// sendText sends one text message (content must be <= 2048 bytes).
func (c *apiClient) sendText(ctx context.Context, openKfID, externalUserID, content string) (string, error) {
	var out sendMsgResponse
	err := c.postJSON(ctx, "send_msg", "/cgi-bin/kf/send_msg", map[string]any{
		"touser":    externalUserID,
		"open_kfid": openKfID,
		"msgtype":   "text",
		"text":      map[string]string{"content": content},
	}, &out)
	if err != nil {
		return "", err
	}
	return out.MsgID, nil
}

// aiMayReply reports whether the API (AI) is allowed to answer in the given
// session state. Queued or human-handled sessions belong to the human
// servicers; ended sessions cannot be answered via API.
func aiMayReply(state int) bool {
	return state == stateUnhandled || state == stateAssistant
}
