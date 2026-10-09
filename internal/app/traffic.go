package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"w103d/gateway/internal/model"
	"w103d/gateway/internal/traffic"
)

type trafficStore struct {
	mu        sync.Mutex
	collector *traffic.Collector
	lastError string
	started   bool
}

type trafficCounter struct {
	Requests      uint64 `json:"requests"`
	ReceivedBytes uint64 `json:"receivedBytes"`
	SentBytes     uint64 `json:"sentBytes"`
	TotalBytes    uint64 `json:"totalBytes"`
}

type trafficDay struct {
	Date string `json:"date"`
	trafficCounter
}

type trafficService struct {
	ServiceID     string         `json:"serviceId"`
	Name          string         `json:"name"`
	Deleted       bool           `json:"deleted"`
	Enabled       bool           `json:"enabled"`
	ServiceStatus string         `json:"serviceStatus"`
	Totals        trafficCounter `json:"totals"`
	Today         trafficCounter `json:"today"`
	LastSeenAt    *string        `json:"lastSeenAt"`
}

type trafficResponse struct {
	Status       string           `json:"status"`
	StartedAt    *string          `json:"startedAt"`
	UpdatedAt    *string          `json:"updatedAt"`
	LastRecordAt *string          `json:"lastRecordAt"`
	Timezone     string           `json:"timezone"`
	Totals       trafficCounter   `json:"totals"`
	Today        trafficCounter   `json:"today"`
	Days         []trafficDay     `json:"days"`
	Services     []trafficService `json:"services"`
	Warnings     []string         `json:"warnings"`
	Incomplete   bool             `json:"incomplete"`
	CatchingUp   bool             `json:"catchingUp"`
}

func trafficTime(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func trafficCount(value traffic.Counters) trafficCounter {
	// The collector validates counters; keep the derived sum from wrapping even
	// if a retained historical snapshot approaches the uint64 ceiling.
	total := value.Received + value.Sent
	if total < value.Received {
		total = ^uint64(0)
	}
	return trafficCounter{Requests: value.Requests, ReceivedBytes: value.Received, SentBytes: value.Sent, TotalBytes: total}
}

func (a *App) trafficCollector() (*traffic.Collector, error) {
	a.traffic.mu.Lock()
	defer a.traffic.mu.Unlock()
	if a.traffic.collector == nil {
		collector, err := traffic.New(a.opts.DataDir)
		if err != nil {
			a.traffic.lastError = "流量统计存储无法读取，原文件已保留；未重新从零累计。"
			return nil, err
		}
		a.traffic.collector = collector
	}
	return a.traffic.collector, nil
}

func (a *App) pollTraffic(ctx context.Context) {
	if a.opts.DryRun || ctx.Err() != nil {
		return
	}
	collector, err := a.trafficCollector()
	if err == nil {
		err = collector.Poll(ctx)
	}
	a.traffic.mu.Lock()
	defer a.traffic.mu.Unlock()
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	if err != nil {
		a.traffic.lastError = "流量采集或统计保存失败，当前显示已保留的累计值；请检查日志和存储。"
	} else {
		a.traffic.lastError = ""
	}
}

func trafficWarning(code string) string {
	// The accounting package keeps stable diagnostic codes. UI prose stays in
	// UTF-8 constants here, with no filesystem paths or raw record contents.
	switch code {
	case "directory_limit", "cursor_limit":
		return "流量日志文件数量达到采集上限，部分文件暂未计入。"
	case "source_unreadable":
		return "部分流量日志缺失、不可读或不是安全的普通文件。"
	case "gzip_invalid", "source_read", "archive_partial":
		return "部分归档日志损坏或含不完整记录，无法完整计量。"
	case "gzip_ambiguous":
		return "压缩归档无法与旧游标唯一对应，已停止重放以避免重复累计。"
	case "archive_horizon":
		return "发现超出恢复期限的未登记归档，未将其重新计入。"
	case "source_changed":
		return "日志被截断或原地替换，相关游标已隔离，避免重复累计。"
	case "source_missing":
		return "曾跟踪的日志在尾部核对完成前消失，统计可能遗漏请求。"
	case "invalid_record":
		return "部分流量日志格式异常或超长，已跳过，累计可能不完整。"
	case "service_limit":
		return "历史服务数量达到统计上限，新增服务记录暂未计入。"
	case "counter_overflow":
		return "流量计数达到数值上限，后续记录未继续累计。"
	default:
		return "流量采集存在异常，请检查日志与统计存储。"
	}
}

func (a *App) startTraffic() {
	if a.opts.DryRun {
		return
	}
	a.traffic.mu.Lock()
	if a.traffic.started {
		a.traffic.mu.Unlock()
		return
	}
	ctx, done, accepted := a.beginWork()
	if !accepted {
		a.traffic.mu.Unlock()
		return
	}
	a.traffic.started = true
	a.traffic.mu.Unlock()
	go func() {
		defer done()
		defer func() {
			a.traffic.mu.Lock()
			collector := a.traffic.collector
			a.traffic.mu.Unlock()
			if collector != nil {
				collector.Close()
			}
		}()
		a.pollTraffic(ctx)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.pollTraffic(ctx)
			}
		}
	}()
}

// This is a configuration-presence check, not a claim that a reload or a live
// request succeeded. Only received log records supply traffic measurements.
func (a *App) trafficConfigured() bool {
	path := filepath.Join(a.opts.DataDir, "managed-nginx.conf")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4<<20))
	return err == nil && strings.Contains(string(data), "gateway_traffic_v1_") && strings.Contains(string(data), "traffic.log")
}

func (a *App) getTraffic(w http.ResponseWriter, r *http.Request) {
	view := trafficResponse{Status: "waiting", Timezone: "Asia/Shanghai", Days: []trafficDay{}, Services: []trafficService{}, Warnings: []string{}}
	if a.opts.DryRun {
		view.Status = "disabled"
		view.Warnings = append(view.Warnings, "开发 dry-run 模式不采集真实流量。")
		reply(w, http.StatusOK, view)
		return
	}
	collector, err := a.trafficCollector()
	if err != nil {
		view.Status = "error"
		view.Warnings = append(view.Warnings, "流量统计存储无法读取，原文件已保留；未重新从零累计。")
		reply(w, http.StatusOK, view)
		return
	}
	snapshot := collector.Snapshot()
	view.StartedAt, view.UpdatedAt, view.LastRecordAt = trafficTime(snapshot.StartedAt), trafficTime(snapshot.UpdatedAt), trafficTime(snapshot.LastRecordAt)
	view.Totals, view.Today = trafficCount(snapshot.Total), trafficCount(snapshot.Today)
	view.Incomplete, view.CatchingUp = snapshot.Incomplete, snapshot.CatchingUp
	started, _ := time.Parse(time.RFC3339Nano, snapshot.StartedAt)
	firstDate := started.In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01-02")
	for _, day := range snapshot.Days {
		// A missing pre-installation day is unknown, not a measured zero. Retained
		// older log records, when present, are still shown after recovery.
		if day.Date < firstDate && day.Total.Requests == 0 {
			continue
		}
		view.Days = append(view.Days, trafficDay{Date: day.Date, trafficCounter: trafficCount(day.Total)})
	}
	for _, warning := range snapshot.Warnings {
		view.Warnings = append(view.Warnings, trafficWarning(warning.Code))
	}
	if view.Incomplete {
		view.Warnings = append(view.Warnings, "部分日志缺失或无法计量，累计只包含成功记录的数据，不代表完整历史。")
	}
	if view.CatchingUp {
		view.Warnings = append(view.Warnings, "正在分批补读历史日志，显示值会随采集继续增加。")
	}

	// Merge current and last-applied metadata without keeping names/URLs in the
	// byte ledger. A deleted service stays visible by its stable ID.
	a.mu.Lock()
	current := append([]model.Service(nil), a.state.Services...)
	appliedRevision := a.state.AppliedRevision
	a.mu.Unlock()
	names := map[string]model.Service{}
	for _, s := range current {
		names[s.ID] = s
	}
	var applied model.State
	var appliedErr error
	if appliedRevision > 0 {
		applied, appliedErr = a.publicAppliedState()
	}
	live := map[string]model.Service{}
	hasActiveService := false
	if appliedErr == nil {
		for _, s := range applied.Services {
			live[s.ID] = s
			hasActiveService = hasActiveService || s.Enabled
			if _, found := names[s.ID]; !found {
				names[s.ID] = s
			}
		}
	} else {
		view.Warnings = append(view.Warnings, "已应用服务信息暂时无法读取，服务状态可能不完整。")
	}
	draftIDs := map[string]bool{}
	for _, s := range current {
		draftIDs[s.ID] = true
	}
	for id := range snapshot.Services {
		if _, found := names[id]; !found {
			names[id] = model.Service{ID: id, Name: id}
		}
	}
	for id, meta := range names {
		entry := snapshot.Services[id]
		serviceStatus := "unknown"
		if !draftIDs[id] {
			serviceStatus = "deleted"
		} else if appliedErr == nil {
			if active, exists := live[id]; !exists {
				serviceStatus = "draft"
			} else if active.Enabled {
				serviceStatus = "active"
			} else {
				serviceStatus = "disabled"
			}
		}
		view.Services = append(view.Services, trafficService{ServiceID: id, Name: meta.Name, Deleted: !draftIDs[id], Enabled: live[id].Enabled,
			ServiceStatus: serviceStatus, Totals: trafficCount(entry.Total), Today: trafficCount(entry.Today), LastSeenAt: trafficTime(entry.LastSeenAt)})
	}
	sort.Slice(view.Services, func(i, j int) bool {
		if view.Services[i].Totals.TotalBytes != view.Services[j].Totals.TotalBytes {
			return view.Services[i].Totals.TotalBytes > view.Services[j].Totals.TotalBytes
		}
		return view.Services[i].ServiceID < view.Services[j].ServiceID
	})
	if snapshot.Total.Requests > 0 {
		view.Status = "collecting"
	}
	if hasActiveService && !a.trafficConfigured() {
		view.Status = "needs-apply"
		view.Warnings = append(view.Warnings, "受管代理尚未配置新版流量日志；升级后需应用配置或按发布说明重启代理，不会自动应用草稿。")
	}
	a.traffic.mu.Lock()
	lastError := a.traffic.lastError
	a.traffic.mu.Unlock()
	if lastError != "" {
		view.Status = "error"
		view.Warnings = append(view.Warnings, lastError)
	}
	reply(w, http.StatusOK, view)
}
