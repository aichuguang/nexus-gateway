package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"w103d/gateway/internal/notify"
)

type notificationFile struct {
	Format   int              `json:"format"`
	Channels []notify.Channel `json:"channels"`
}
type notificationResult struct {
	Status string
	At     string
	Error  string
}
type notificationStore struct {
	mu       sync.Mutex
	loaded   bool
	config   notificationFile
	err      error
	results  map[string]notificationResult
	sent     map[string]bool
	order    []string
	testing  map[string]bool
	lastTest map[string]time.Time
	active   map[string]map[string]context.CancelFunc
	sender   func(context.Context, notify.Channel, notify.Event) error // mock seam; never configurable over HTTP
}

type notificationView struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Type          string   `json:"type"`
	Enabled       bool     `json:"enabled"`
	EndpointHint  string   `json:"endpointHint"`
	HasEndpoint   bool     `json:"hasEndpoint"`
	HasSecret     bool     `json:"hasSecret"`
	SMTPHost      string   `json:"smtpHost"`
	SMTPPort      int      `json:"smtpPort"`
	SMTPUsername  string   `json:"smtpUsername"`
	SMTPFrom      string   `json:"smtpFrom"`
	SMTPTo        []string `json:"smtpTo"`
	SMTPTLS       string   `json:"smtpTLS"`
	HasPassword   bool     `json:"hasPassword"`
	LastStatus    string   `json:"lastStatus"`
	LastAttemptAt string   `json:"lastAttemptAt"`
	LastError     string   `json:"lastError"`
}

type notificationInput struct {
	Name         string   `json:"name"`
	Type         string   `json:"type"`
	Enabled      bool     `json:"enabled"`
	Endpoint     string   `json:"endpoint"`
	Secret       string   `json:"secret"`
	SMTPHost     string   `json:"smtpHost"`
	SMTPPort     int      `json:"smtpPort"`
	SMTPUsername string   `json:"smtpUsername"`
	SMTPPassword string   `json:"smtpPassword"`
	SMTPFrom     string   `json:"smtpFrom"`
	SMTPTo       []string `json:"smtpTo"`
	SMTPTLS      string   `json:"smtpTLS"`
}

func readPrivateSettingsJSON(path string, value any, limit int64) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return errors.New("设置文件类型或大小不合法")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return errors.New("设置文件无法读取")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil {
		return errors.New("设置文件格式无效")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return errors.New("设置文件存在多余内容")
	}
	return nil
}

func (a *App) loadNotificationsLocked() {
	if a.notifications.loaded {
		return
	}
	a.notifications.loaded = true
	a.notifications.config = notificationFile{Format: 1, Channels: []notify.Channel{}}
	a.notifications.results = map[string]notificationResult{}
	a.notifications.sent = map[string]bool{}
	a.notifications.testing = map[string]bool{}
	a.notifications.lastTest = map[string]time.Time{}
	a.notifications.active = map[string]map[string]context.CancelFunc{}
	var stored notificationFile
	err := readPrivateSettingsJSON(filepath.Join(a.opts.DataDir, "notifications.json"), &stored, 128<<10)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil || stored.Format != 1 || len(stored.Channels) > 10 {
		a.notifications.err = errors.New("通知设置无法读取或版本不兼容，原文件未被重置")
		return
	}
	seen := map[string]bool{}
	for _, channel := range stored.Channels {
		if !validID.MatchString(channel.ID) || seen[channel.ID] || channel.Revision < 1 || notify.Validate(channel) != nil {
			a.notifications.err = errors.New("通知设置内容不合法，已停止发送")
			return
		}
		seen[channel.ID] = true
	}
	if stored.Channels == nil {
		stored.Channels = []notify.Channel{}
	}
	a.notifications.config = stored
}

func (a *App) notificationViewsLocked() []notificationView {
	views := []notificationView{}
	for _, channel := range a.notifications.config.Channels {
		hint := ""
		if u, err := url.Parse(channel.Endpoint); err == nil {
			hint = u.Hostname()
		}
		result := a.notifications.results[channel.ID]
		if result.Status == "" {
			result.Status = "unknown"
		}
		views = append(views, notificationView{ID: channel.ID, Name: channel.Name, Type: channel.Type, Enabled: channel.Enabled, EndpointHint: hint, HasEndpoint: channel.Endpoint != "", HasSecret: channel.Secret != "", SMTPHost: channel.SMTPHost, SMTPPort: channel.SMTPPort, SMTPUsername: channel.SMTPUsername, SMTPFrom: channel.SMTPFrom, SMTPTo: append([]string{}, channel.SMTPTo...), SMTPTLS: channel.SMTPTLS, HasPassword: channel.SMTPPassword != "", LastStatus: result.Status, LastAttemptAt: result.At, LastError: result.Error})
	}
	return views
}

func (a *App) listNotificationChannels(w http.ResponseWriter, r *http.Request) {
	a.notifications.mu.Lock()
	defer a.notifications.mu.Unlock()
	a.loadNotificationsLocked()
	if a.notifications.err != nil {
		fail(w, 503, a.notifications.err.Error())
		return
	}
	reply(w, 200, map[string]any{"channels": a.notificationViewsLocked()})
}

func (a *App) createNotificationChannel(w http.ResponseWriter, r *http.Request) {
	a.saveNotificationChannel(w, r, true)
}
func (a *App) updateNotificationChannel(w http.ResponseWriter, r *http.Request) {
	a.saveNotificationChannel(w, r, false)
}
func (a *App) saveNotificationChannel(w http.ResponseWriter, r *http.Request, create bool) {
	var in notificationInput
	if !decode(w, r, &in) {
		return
	}
	a.notifications.mu.Lock()
	defer a.notifications.mu.Unlock()
	a.loadNotificationsLocked()
	if a.notifications.err != nil {
		fail(w, 503, a.notifications.err.Error())
		return
	}
	index := -1
	channel := notify.Channel{ID: NewID(), Revision: 1}
	if create {
		if len(a.notifications.config.Channels) >= 10 {
			fail(w, 400, "通知渠道数量已达上限")
			return
		}
	} else {
		for i, c := range a.notifications.config.Channels {
			if c.ID == r.PathValue("id") {
				index = i
				channel = c
				break
			}
		}
		if index < 0 {
			fail(w, 404, "通知渠道不存在")
			return
		}
		if channel.Type != in.Type {
			fail(w, 400, "渠道类型不能更改，请新建另一个渠道")
			return
		}
		channel.Revision++
	}
	channel.Name, channel.Type, channel.Enabled = strings.TrimSpace(in.Name), in.Type, in.Enabled
	if in.Endpoint != "" {
		channel.Endpoint = strings.TrimSpace(in.Endpoint)
	}
	if in.Secret != "" {
		channel.Secret = in.Secret
	}
	if in.SMTPPassword != "" {
		channel.SMTPPassword = in.SMTPPassword
	}
	channel.SMTPHost, channel.SMTPPort, channel.SMTPUsername = strings.TrimSpace(in.SMTPHost), in.SMTPPort, strings.TrimSpace(in.SMTPUsername)
	channel.SMTPFrom, channel.SMTPTo, channel.SMTPTLS = strings.TrimSpace(in.SMTPFrom), append([]string{}, in.SMTPTo...), in.SMTPTLS
	if channel.Type == "smtp" {
		channel.Endpoint, channel.Secret = "", ""
	} else {
		channel.SMTPHost, channel.SMTPUsername, channel.SMTPPassword, channel.SMTPFrom, channel.SMTPTLS = "", "", "", "", ""
		channel.SMTPPort = 0
		channel.SMTPTo = nil
	}
	if err := notify.Validate(channel); err != nil {
		fail(w, 400, err.Error())
		return
	}
	next := notificationFile{Format: 1, Channels: append([]notify.Channel{}, a.notifications.config.Channels...)}
	if create {
		next.Channels = append(next.Channels, channel)
	} else {
		next.Channels[index] = channel
	}
	if writeJSON(filepath.Join(a.opts.DataDir, "notifications.json"), next) != nil {
		fail(w, 500, "无法安全保存通知设置")
		return
	}
	a.notifications.config = next
	a.cancelNotificationDeliveriesLocked(channel.ID)
	delete(a.notifications.results, channel.ID)
	reply(w, 200, map[string]any{"channels": a.notificationViewsLocked()})
}

func (a *App) deleteNotificationChannel(w http.ResponseWriter, r *http.Request) {
	a.notifications.mu.Lock()
	defer a.notifications.mu.Unlock()
	a.loadNotificationsLocked()
	if a.notifications.err != nil {
		fail(w, 503, a.notifications.err.Error())
		return
	}
	channels := []notify.Channel{}
	found := false
	for _, channel := range a.notifications.config.Channels {
		if channel.ID == r.PathValue("id") {
			found = true
			continue
		}
		channels = append(channels, channel)
	}
	if !found {
		fail(w, 404, "通知渠道不存在")
		return
	}
	next := notificationFile{Format: 1, Channels: channels}
	if writeJSON(filepath.Join(a.opts.DataDir, "notifications.json"), next) != nil {
		fail(w, 500, "无法保存通知设置")
		return
	}
	a.notifications.config = next
	a.cancelNotificationDeliveriesLocked(r.PathValue("id"))
	delete(a.notifications.results, r.PathValue("id"))
	delete(a.notifications.lastTest, r.PathValue("id"))
	reply(w, 200, map[string]any{"channels": a.notificationViewsLocked()})
}

// A saved pause, credential change, or deletion invalidates both queued retries
// and current network I/O. Already accepted deliveries cannot be recalled.
func (a *App) cancelNotificationDeliveriesLocked(id string) {
	for _, cancel := range a.notifications.active[id] {
		cancel()
	}
}

func (a *App) deliverNotification(ctx context.Context, channel notify.Channel, event notify.Event) error {
	if a.opts.DryRun {
		return errors.New("开发 dry-run 模式禁止发送真实通知")
	}
	a.notifications.mu.Lock()
	a.loadNotificationsLocked()
	currentConfig := false
	for _, current := range a.notifications.config.Channels {
		if current.ID == channel.ID && current.Revision == channel.Revision && (current.Enabled || event.Kind == "test") {
			currentConfig = true
			break
		}
	}
	if a.notifications.err != nil || !currentConfig {
		a.notifications.mu.Unlock()
		return errors.New("通知渠道配置已变化，本次旧配置发送已取消")
	}
	key := channel.ID + ":" + event.ID
	if a.notifications.sent[key] {
		a.notifications.mu.Unlock()
		return nil
	}
	a.notifications.sent[key] = true
	ctx, cancel := context.WithCancel(ctx)
	if a.notifications.active[channel.ID] == nil {
		a.notifications.active[channel.ID] = map[string]context.CancelFunc{}
	}
	a.notifications.active[channel.ID][key] = cancel
	defer func() {
		cancel()
		a.notifications.mu.Lock()
		delete(a.notifications.active[channel.ID], key)
		if len(a.notifications.active[channel.ID]) == 0 {
			delete(a.notifications.active, channel.ID)
		}
		a.notifications.mu.Unlock()
	}()
	a.notifications.order = append(a.notifications.order, key)
	if len(a.notifications.order) > 2000 {
		delete(a.notifications.sent, a.notifications.order[0])
		a.notifications.order = a.notifications.order[1:]
	}
	send := a.notifications.sender
	a.notifications.mu.Unlock()
	if send == nil {
		send = notify.Send
	}
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if ctx.Err() != nil {
			err = ctx.Err()
			break
		}
		err = send(ctx, channel, event)
		if err == nil || !notify.Retryable(err) || attempt == 1 {
			break
		}
		timer := time.NewTimer(300 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			err = ctx.Err()
		case <-timer.C:
		}
	}
	result := notificationResult{Status: "ok", At: time.Now().UTC().Format(time.RFC3339)}
	if err != nil {
		result.Status = "error"
		result.Error = "通知发送失败，请检查渠道配置或稍后显式测试"
	}
	a.notifications.mu.Lock()
	for _, current := range a.notifications.config.Channels {
		if current.ID == channel.ID && current.Revision == channel.Revision {
			a.notifications.results[channel.ID] = result
			break
		}
	}
	a.notifications.mu.Unlock()
	return err
}

func (a *App) notifyMonitorEvent(ctx context.Context, event notify.Event) {
	a.notifications.mu.Lock()
	a.loadNotificationsLocked()
	if a.notifications.err != nil {
		a.notifications.mu.Unlock()
		return
	}
	channels := append([]notify.Channel{}, a.notifications.config.Channels...)
	a.notifications.mu.Unlock()
	var workers sync.WaitGroup
	semaphore := make(chan struct{}, 2)
	for _, channel := range channels {
		if !channel.Enabled {
			continue
		}
		workers.Add(1)
		go func(channel notify.Channel) {
			defer workers.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				return
			}
			_ = a.deliverNotification(ctx, channel, event)
		}(channel)
	}
	workers.Wait()
}

func (a *App) testNotificationChannel(w http.ResponseWriter, r *http.Request) {
	var in struct{}
	if !decode(w, r, &in) {
		return
	}
	if a.opts.DryRun {
		fail(w, 403, "开发 dry-run 模式禁止发送真实通知")
		return
	}
	workCtx, finish, accepted := a.beginWork()
	if !accepted {
		fail(w, 503, errAppStopping.Error())
		return
	}
	defer finish()
	a.notifications.mu.Lock()
	a.loadNotificationsLocked()
	if a.notifications.err != nil {
		err := a.notifications.err
		a.notifications.mu.Unlock()
		fail(w, 503, err.Error())
		return
	}
	var channel notify.Channel
	for _, candidate := range a.notifications.config.Channels {
		if candidate.ID == r.PathValue("id") {
			channel = candidate
			break
		}
	}
	if channel.ID == "" {
		a.notifications.mu.Unlock()
		fail(w, 404, "通知渠道不存在")
		return
	}
	if a.notifications.testing[channel.ID] {
		a.notifications.mu.Unlock()
		fail(w, 409, "该渠道正在测试，请稍后查看结果")
		return
	}
	if time.Since(a.notifications.lastTest[channel.ID]) < 30*time.Second {
		a.notifications.mu.Unlock()
		fail(w, 429, "测试通知每个渠道至少间隔 30 秒，请稍后重试")
		return
	}
	a.notifications.lastTest[channel.ID] = time.Now()
	a.notifications.testing[channel.ID] = true
	a.notifications.mu.Unlock()
	defer func() {
		a.notifications.mu.Lock()
		delete(a.notifications.testing, channel.ID)
		a.notifications.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(workCtx, 18*time.Second)
	defer cancel()
	stopRequestCancellation := context.AfterFunc(r.Context(), cancel)
	defer stopRequestCancellation()
	if r.Context().Err() != nil {
		cancel()
	}
	event := notify.Event{ID: NewID(), Kind: "test", Status: "ok", At: time.Now().UTC().Format(time.RFC3339), Message: "网关通知渠道测试：收到此消息说明渠道已接受测试通知。"}
	if a.deliverNotification(ctx, channel, event) != nil {
		fail(w, 502, "通知测试失败，请检查网络、机器人权限、关键词、签名或 SMTP 设置；密钥不会在错误中回显")
		return
	}
	reply(w, 200, map[string]bool{"ok": true})
}
