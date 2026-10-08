package wecomkf

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const defaultAPIBaseURL = "https://qyapi.weixin.qq.com"

// Client calls the WeCom Customer Service APIs. It contains no credentials;
// callers supply the short-lived access token for each request.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

func NewClient(baseURL string, httpClient *http.Client) *Client {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultAPIBaseURL
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), httpClient: httpClient}
}

type SyncMessageRequest struct {
	Cursor  string `json:"cursor,omitempty"`
	Token   string `json:"token"`
	Limit   int    `json:"limit,omitempty"`
	OpenKfID string `json:"open_kfid"`
}

type SyncMessageResponse struct {
	ErrCode    int           `json:"errcode"`
	ErrMsg     string        `json:"errmsg"`
	NextCursor string        `json:"next_cursor"`
	HasMore    BoolInt       `json:"has_more"`
	MsgList    []SyncMessage  `json:"msg_list"`
}

type BoolInt bool

func (b *BoolInt) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return fmt.Errorf("wecomkf: invalid boolean/integer flag %q", string(data))
	}
	if string(data) == "true" {
		*b = true
		return nil
	}
	if string(data) == "false" {
		*b = false
		return nil
	}
	var n int
	if err := json.Unmarshal(data, &n); err != nil {
		return fmt.Errorf("wecomkf: invalid boolean/integer flag %q: %w", string(data), err)
	}
	*b = n != 0
	return nil
}

type SyncMessage struct {
	MsgID          string           `json:"msgid"`
	OpenKfID       string           `json:"open_kfid"`
	ExternalUserID string           `json:"external_userid"`
	SendTime       int64            `json:"send_time"`
	Origin         int              `json:"origin"`
	MsgType        string           `json:"msgtype"`
	Text           *MessageText     `json:"text,omitempty"`
}

type MessageText struct {
	Content string `json:"content"`
}

type SendTextRequest struct {
	ToUser  string      `json:"touser"`
	OpenKfID string      `json:"open_kfid"`
	MsgID   string      `json:"msgid,omitempty"`
	MsgType string      `json:"msgtype"`
	Text    MessageText `json:"text"`
}

type APIError struct {
	Code int
	Msg  string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("wecomkf: API error %d: %s", e.Code, e.Msg)
}

func (c *Client) SyncMessages(ctx context.Context, accessToken string, req SyncMessageRequest) (SyncMessageResponse, error) {
	var out SyncMessageResponse
	if req.Limit <= 0 || req.Limit > 1000 {
		req.Limit = 1000
	}
	if err := c.postJSON(ctx, "/cgi-bin/kf/sync_msg", accessToken, req, &out); err != nil {
		return SyncMessageResponse{}, err
	}
	return out, nil
}

func (c *Client) SendText(ctx context.Context, accessToken string, req SendTextRequest) error {
	if req.MsgType == "" {
		req.MsgType = "text"
	}
	if req.ToUser == "" || req.OpenKfID == "" || req.Text.Content == "" {
		return fmt.Errorf("wecomkf: touser, open_kfid and text.content are required")
	}
	return c.postJSON(ctx, "/cgi-bin/kf/send_msg", accessToken, req, &struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}{})
}

func (c *Client) postJSON(ctx context.Context, path, accessToken string, request, response any) error {
	if strings.TrimSpace(accessToken) == "" {
		return fmt.Errorf("wecomkf: access token is required")
	}
	body, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("wecomkf: encode %s: %w", path, err)
	}
	u, err := url.Parse(c.baseURL + path)
	if err != nil {
		return fmt.Errorf("wecomkf: build %s URL: %w", path, err)
	}
	q := u.Query()
	q.Set("access_token", accessToken)
	u.RawQuery = q.Encode()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("wecomkf: create %s request: %w", path, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("wecomkf: request %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("wecomkf: read %s response: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("wecomkf: HTTP %d from %s", resp.StatusCode, path)
	}
	if err := json.Unmarshal(data, response); err != nil {
		return fmt.Errorf("wecomkf: decode %s response: %w", path, err)
	}
	var envelope struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(data, &envelope); err == nil && envelope.ErrCode != 0 {
		return &APIError{Code: envelope.ErrCode, Msg: envelope.ErrMsg}
	}
	return nil
}
