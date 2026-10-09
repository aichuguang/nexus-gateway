// Package notify delivers bounded notifications without exposing credentials in errors.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type Channel struct {
	ID           string   `json:"id"`
	Revision     int64    `json:"revision"`
	Name         string   `json:"name"`
	Type         string   `json:"type"`
	Enabled      bool     `json:"enabled"`
	Endpoint     string   `json:"endpoint,omitempty"`
	Secret       string   `json:"secret,omitempty"`
	SMTPHost     string   `json:"smtpHost,omitempty"`
	SMTPPort     int      `json:"smtpPort,omitempty"`
	SMTPUsername string   `json:"smtpUsername,omitempty"`
	SMTPPassword string   `json:"smtpPassword,omitempty"`
	SMTPFrom     string   `json:"smtpFrom,omitempty"`
	SMTPTo       []string `json:"smtpTo,omitempty"`
	SMTPTLS      string   `json:"smtpTLS,omitempty"`
}

type Event struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	ServiceID   string `json:"serviceId,omitempty"`
	ServiceName string `json:"serviceName,omitempty"`
	Status      string `json:"status"`
	At          string `json:"at"`
	Message     string `json:"message"`
}

type DeliveryError struct {
	Message   string
	Temporary bool
}

func (e *DeliveryError) Error() string { return e.Message }
func Retryable(err error) bool {
	var delivery *DeliveryError
	return errors.As(err, &delivery) && delivery.Temporary
}

func Send(ctx context.Context, channel Channel, event Event) error {
	if err := Validate(channel); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if channel.Type == "smtp" {
		return sendSMTP(ctx, channel, event)
	}
	transport := &http.Transport{Proxy: nil, DialContext: publicDialContext, TLSHandshakeTimeout: 4 * time.Second, ResponseHeaderTimeout: 5 * time.Second, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return sendHTTP(ctx, client, channel, event)
}

func sendHTTP(ctx context.Context, client *http.Client, channel Channel, event Event) error {
	request, err := buildRequest(ctx, channel, event, time.Now())
	if err != nil {
		return errors.New("无法构造通知请求")
	}
	response, err := client.Do(request)
	if err != nil {
		return &DeliveryError{Message: "通知连接失败或超时", Temporary: ctx.Err() == nil}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &DeliveryError{Message: fmt.Sprintf("通知渠道返回 HTTP %d", response.StatusCode), Temporary: response.StatusCode == 429 || response.StatusCode >= 500}
	}
	if channel.Type == "webhook" {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil || len(data) > 65536 {
		return errors.New("通知渠道响应不可读取，结果需核对")
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(data, &result) != nil {
		return errors.New("通知渠道响应格式无效，结果需核对")
	}
	key := "errcode"
	if channel.Type == "feishu" {
		key = "code"
		if _, found := result[key]; !found {
			key = "StatusCode"
		}
	}
	var code int
	if raw, found := result[key]; !found || string(raw) == "null" || json.Unmarshal(raw, &code) != nil {
		return errors.New("通知渠道未确认接收，结果需核对")
	}
	if code != 0 {
		return fmt.Errorf("通知渠道拒绝请求（代码 %d），请检查机器人权限、关键词或签名", code)
	}
	return nil
}

func buildRequest(ctx context.Context, c Channel, event Event, now time.Time) (*http.Request, error) {
	endpoint := c.Endpoint
	var payload any = event
	if c.Type == "wecom" || c.Type == "dingtalk" {
		payload = map[string]any{"msgtype": "text", "text": map[string]string{"content": event.Message}}
	}
	if c.Type == "feishu" {
		body := map[string]any{"msg_type": "text", "content": map[string]string{"text": event.Message}}
		if c.Secret != "" {
			timestamp := strconv.FormatInt(now.Unix(), 10)
			mac := hmac.New(sha256.New, []byte(timestamp+"\n"+c.Secret))
			body["timestamp"], body["sign"] = timestamp, base64.StdEncoding.EncodeToString(mac.Sum(nil))
		}
		payload = body
	}
	if c.Type == "dingtalk" && c.Secret != "" {
		timestamp := strconv.FormatInt(now.UnixMilli(), 10)
		mac := hmac.New(sha256.New, []byte(c.Secret))
		_, _ = mac.Write([]byte(timestamp + "\n" + c.Secret))
		u, err := url.Parse(endpoint)
		if err != nil {
			return nil, err
		}
		query := u.Query()
		query.Set("timestamp", timestamp)
		query.Set("sign", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
		u.RawQuery = query.Encode()
		endpoint = u.String()
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	request.Header.Set("User-Agent", "Gateway-Notifications/1")
	request.Header.Set("X-Gateway-Event-ID", event.ID)
	if c.Type == "webhook" && c.Secret != "" {
		request.Header.Set("Authorization", "Bearer "+c.Secret)
	}
	return request, nil
}
