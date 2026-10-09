package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"w103d/gateway/internal/model"
)

const publishJobFormat = 1
const publishJobLimit = 100
const publishHistoryLimit = 20
const publishJobFileLimit = 4 << 20

var publishRequestID = regexp.MustCompile(`^[A-Za-z0-9_-]{16,80}$`)
var errGatewayChanging = errors.New("网关正在执行发布或配置变更")

type publishJobStep struct {
	publishStep
	Status    string `json:"status"`
	UpdatedAt string `json:"updatedAt"`
}

type publishJob struct {
	ID                      string           `json:"id"`
	RequestID               string           `json:"requestId"`
	ServiceID               string           `json:"serviceId"`
	ServiceName             string           `json:"serviceName"`
	Edge                    bool             `json:"edge"`
	EdgeProvider            string           `json:"edgeProvider,omitempty"`
	DNS                     bool             `json:"dns"`
	Revision                int64            `json:"revision"`
	PublishFingerprint      string           `json:"publishFingerprint"`
	Status                  string           `json:"status"`
	CreatedAt               string           `json:"createdAt"`
	UpdatedAt               string           `json:"updatedAt"`
	Message                 string           `json:"message,omitempty"`
	Steps                   []publishJobStep `json:"steps"`
	ContinuationFingerprint string           `json:"continuationFingerprint,omitempty"`
	WaitStartedAt           string           `json:"waitStartedAt,omitempty"`
	NextAttemptAt           string           `json:"nextAttemptAt,omitempty"`
	Attempts                int              `json:"attempts,omitempty"`
}

type publishJobLog struct {
	Format int          `json:"format"`
	Jobs   []publishJob `json:"jobs"`
}

// This lock never waits for a cloud call or takes App.mu. Progress must remain
// readable even while the existing cloud implementation holds the business lock.
type publishJobStore struct {
	mu     sync.Mutex
	loaded bool
	log    publishJobLog
	err    error
	cancel context.CancelFunc
	done   chan struct{}
	write  func(publishJobLog) error // test seam for persistence failures
}

func publishFingerprint(s model.State) string {
	// Exclude results and timestamps, but include settings and credential versions:
	// those can change a cloud write without changing the proxy's generation.
	s.AppliedAt, s.AppliedPublicAddress = "", ""
	s.AppliedRevision = 0
	s.Certificates = append([]model.Certificate{}, s.Certificates...)
	for i := range s.Certificates {
		s.Certificates[i].EdgeOne = nil
	}
	s.EdgeOrigins = nil
	encoded, _ := json.Marshal(s)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func (a *App) loadPublishJobsLocked() {
	if a.jobs.loaded {
		return
	}
	a.jobs.loaded = true
	a.jobs.log = publishJobLog{Format: publishJobFormat, Jobs: []publishJob{}}
	p := filepath.Join(a.opts.DataDir, "publish-jobs.json")
	info, err := os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > publishJobFileLimit {
		a.jobs.err = errors.New("无法读取发布记录；请检查数据目录，记录未被重置")
		return
	}
	data, err := os.ReadFile(p)
	var history publishJobLog
	if err != nil || json.Unmarshal(data, &history) != nil || history.Format != publishJobFormat || len(history.Jobs) > publishJobLimit {
		a.jobs.err = errors.New("发布记录损坏或格式不兼容；已停止接受新发布，原文件保持不变")
		return
	}
	seen := map[string]bool{}
	seenIDs := map[string]bool{}
	changed := false
	for i := range history.Jobs {
		job := &history.Jobs[i]
		if job.ID == "" || seenIDs[job.ID] || seen[job.RequestID] || !publishRequestID.MatchString(job.RequestID) || !validJobStatus(job.Status) {
			a.jobs.err = errors.New("发布记录内容不合法；原文件保持不变")
			return
		}
		seen[job.RequestID] = true
		seenIDs[job.ID] = true
		if receipt, err := a.readOperationReceipt(job.RequestID); err == nil {
			if receipt.Kind != "publish" || receipt.Job == nil || receipt.InputDigest != publishReceiptDigest(*job) {
				a.jobs.err = errors.New("发布历史与持久收据冲突")
				return
			}
			if publishJobActive(job.Status) || job.Status == "interrupted" {
				*job = *receipt.Job
				changed = true
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			a.jobs.err = err
			return
		}
		if job.Status == "running" {
			job.Status = "interrupted"
			job.Message = "管理程序在发布期间停止；请核对本机与云端状态，未自动重试"
			job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			for j := range job.Steps {
				if job.Steps[j].Status == "running" {
					job.Steps[j].Status = "unknown"
				}
			}
			changed = true
		}
		if err := a.savePublishReceipt(*job); err != nil {
			a.jobs.err = errors.New("无法迁移或更新持久发布收据，原任务历史保持不变")
			return
		}
	}
	if history.Jobs == nil {
		history.Jobs = []publishJob{}
	}
	a.jobs.log = history
	if changed {
		_ = a.savePublishJobsLocked()
	}
}

func validJobStatus(status string) bool {
	switch status {
	case "running", "waiting", "succeeded", "failed", "partial_failed", "needs_review", "interrupted":
		return true
	}
	return false
}

func (a *App) savePublishJobsLocked() error {
	if a.jobs.err != nil {
		return a.jobs.err
	}
	for _, job := range a.jobs.log.Jobs {
		if err := a.savePublishReceipt(job); err != nil {
			a.jobs.err = errors.New("无法保存持久发布收据，已停止后续操作")
			return a.jobs.err
		}
	}
	data, err := json.Marshal(a.jobs.log)
	if err == nil && len(data) > publishJobFileLimit {
		err = errors.New("publish history too large")
	}
	if err == nil {
		if a.jobs.write != nil {
			err = a.jobs.write(a.jobs.log)
		} else {
			err = AtomicWrite(filepath.Join(a.opts.DataDir, "publish-jobs.json"), append(data, '\n'))
		}
	}
	if err != nil {
		a.jobs.err = errors.New("发布记录无法保存；已停止后续操作，请检查磁盘并核对最后一步的实际状态")
	}
	return a.jobs.err
}

func (a *App) listPublishJobs(w http.ResponseWriter, r *http.Request) {
	a.jobs.mu.Lock()
	defer a.jobs.mu.Unlock()
	a.loadPublishJobsLocked()
	message := ""
	if a.jobs.err != nil {
		message = a.jobs.err.Error()
	}
	reply(w, 200, map[string]any{"jobs": a.jobs.log.Jobs, "error": message})
}

// The gate covers the complete write, not a check before a handler later waits
// for mu. Background writers take it in the same order: changeMu -> edge/dns -> mu.
func (a *App) changing(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.lifecycleContext().Err() != nil {
			fail(w, http.StatusServiceUnavailable, errAppStopping.Error())
			return
		}
		if !a.changeMu.TryLock() {
			fail(w, http.StatusConflict, "网关正在执行发布或配置变更，请稍后再修改；可以继续查看进度")
			return
		}
		defer a.changeMu.Unlock()
		if err := a.sealCompletedCreations(); err != nil {
			fail(w, 503, "无法结算已完成创建收据，已阻止后续修改，请检查操作记录存储")
			return
		}
		if r.Context().Err() != nil {
			fail(w, http.StatusServiceUnavailable, errAppStopping.Error())
			return
		}
		handler(w, r)
	}
}

func (a *App) createPublishJob(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Revision           int64  `json:"revision"`
		PublishFingerprint string `json:"publishFingerprint"`
		RequestID          string `json:"requestId"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !publishRequestID.MatchString(in.RequestID) || len(in.PublishFingerprint) != 64 {
		fail(w, 400, "发布请求标识不正确，请重新预览")
		return
	}
	serviceID := r.PathValue("id")
	// Deduplication does not take the business lock: a response lost while a cloud
	// call is running must still be recoverable immediately.
	a.jobs.mu.Lock()
	a.loadPublishJobsLocked()
	if receipt, err := a.readOperationReceipt(in.RequestID); err == nil {
		candidate := publishJob{ServiceID: serviceID, Revision: in.Revision, PublishFingerprint: in.PublishFingerprint}
		if receipt.Kind != "publish" || receipt.InputDigest != publishReceiptDigest(candidate) || receipt.Job == nil {
			fail(w, 409, "同一请求标识不能携带不同配置或操作")
		} else {
			job := *receipt.Job
			if job.Status == "running" && a.jobs.done == nil {
				job.Status, job.Message = "interrupted", "发布曾中断，请核对原操作，不会重复执行"
			}
			reply(w, 202, map[string]any{"job": job})
		}
		a.jobs.mu.Unlock()
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		fail(w, 503, "无法读取原发布收据，未执行发布")
		a.jobs.mu.Unlock()
		return
	}
	for _, job := range a.jobs.log.Jobs {
		if job.RequestID == in.RequestID {
			if job.ServiceID != serviceID || job.Revision != in.Revision || job.PublishFingerprint != in.PublishFingerprint {
				fail(w, 409, "同一发布请求不能携带不同配置，请重新预览")
			} else {
				reply(w, 202, map[string]any{"job": job})
			}
			a.jobs.mu.Unlock()
			return
		}
	}
	if a.jobs.err != nil {
		fail(w, 503, a.jobs.err.Error())
		a.jobs.mu.Unlock()
		return
	}
	a.jobs.mu.Unlock()
	if !a.changeMu.TryLock() {
		fail(w, 409, "已有发布或配置变更正在执行，请先查看当前进度")
		return
	}
	parent, finish, admitted := a.beginWork()
	if !admitted {
		a.changeMu.Unlock()
		fail(w, http.StatusServiceUnavailable, errAppStopping.Error())
		return
	}
	accepted := false
	defer func() {
		if !accepted {
			a.changeMu.Unlock()
			finish()
		}
	}()
	if !lockContext(parent, &a.mu) {
		fail(w, http.StatusServiceUnavailable, errAppStopping.Error())
		return
	}
	defer a.mu.Unlock()
	// Once the write gate is ours, a definite preflight rejection is durable as
	// well. A delayed copy of this request must see the same refusal, not become
	// a new publish after the browser discarded its pending context.
	if receipt, e := a.readOperationReceipt(in.RequestID); e == nil {
		candidate := publishJob{ServiceID: serviceID, Revision: in.Revision, PublishFingerprint: in.PublishFingerprint}
		if receipt.Kind == "publish" && receipt.Job != nil && receipt.InputDigest == publishReceiptDigest(candidate) {
			reply(w, 202, map[string]any{"job": receipt.Job})
		} else {
			fail(w, 409, "请求标识已用于其他操作")
		}
		return
	} else if !errors.Is(e, os.ErrNotExist) {
		fail(w, 503, "无法读取原请求收据")
		return
	}
	reject := func(code int, message string) {
		now := time.Now().UTC().Format(time.RFC3339)
		job := publishJob{ID: NewID(), RequestID: in.RequestID, ServiceID: serviceID, Revision: in.Revision, PublishFingerprint: in.PublishFingerprint, Status: "failed", CreatedAt: now, UpdatedAt: now, Message: message, Steps: []publishJobStep{}}
		if e := a.savePublishReceipt(job); e != nil {
			fail(w, 503, "无法保存确定拒绝的收据，请保留原请求标识核对")
			return
		}
		fail(w, code, message)
	}
	index := a.serviceIndex(serviceID)
	if index < 0 {
		reject(404, "服务不存在，请刷新页面")
		return
	}
	if in.Revision != a.state.Revision || in.PublishFingerprint != publishFingerprint(a.state) {
		reject(409, "配置已变化，请重新预览后发布")
		return
	}
	if err := a.refuseDeletionProtectedState(a.state); err != nil {
		reject(409, err.Error())
		return
	}
	service := a.state.Services[index]
	if _, _, err := a.engine.Render(a.state, a.publicAddressForRender()); err != nil {
		reject(400, err.Error())
		return
	}
	if err := edgeServiceAccountAvailable(a.state, service); err != nil {
		reject(400, err.Error())
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	job := publishJob{ID: NewID(), RequestID: in.RequestID, ServiceID: service.ID, ServiceName: service.Name,
		EdgeProvider: service.EffectiveEdgeProvider(),
		Edge:         service.Enabled && service.ServesEdge(), DNS: service.UsesDomains() || a.baseDomain() != "",
		Revision: in.Revision, PublishFingerprint: in.PublishFingerprint, Status: "running", CreatedAt: now, UpdatedAt: now, Steps: []publishJobStep{}}
	if service.ServesCloudflare() && service.Enabled {
		job.ContinuationFingerprint = publishContinuationFingerprint(a.state, service.ID)
	}
	a.jobs.mu.Lock()
	defer a.jobs.mu.Unlock()
	// A concurrent identical request may have passed the first lookup before the
	// previous worker finished. Check again under the reserved write gate.
	if receipt, err := a.readOperationReceipt(in.RequestID); err == nil {
		if receipt.Kind != "publish" || receipt.Job == nil || receipt.InputDigest != publishReceiptDigest(job) {
			fail(w, 409, "请求标识已用于其他操作")
		} else {
			reply(w, 202, map[string]any{"job": receipt.Job})
		}
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		fail(w, 503, "无法读取原操作收据")
		return
	}
	for _, existing := range a.jobs.log.Jobs {
		if existing.RequestID == in.RequestID {
			if existing.ServiceID != serviceID || existing.Revision != in.Revision || existing.PublishFingerprint != in.PublishFingerprint {
				fail(w, 409, "同一发布请求不能携带不同配置，请重新预览")
			} else {
				reply(w, 202, map[string]any{"job": existing})
			}
			return
		}
	}
	kept, ordinary := []publishJob{}, 0
	for i := range a.jobs.log.Jobs {
		previous := &a.jobs.log.Jobs[i]
		if previous.ServiceID != serviceID || previous.Status != "waiting" {
			continue
		}
		if previous.ContinuationFingerprint == job.ContinuationFingerprint {
			reject(409, "该服务正在自动等待生效，请查看现有进度，无需重复发布")
			return
		}
		previous.Status, previous.Message, previous.NextAttemptAt = "partial_failed", "服务配置已更新，旧等待任务已由新发布替代", ""
		previous.UpdatedAt = now
	}
	for _, previous := range a.jobs.log.Jobs {
		protected := publishJobActive(previous.Status) || previous.Status == "needs_review" || previous.Status == "interrupted"
		if protected || ordinary < publishHistoryLimit-1 {
			kept = append(kept, previous)
		}
		if !protected {
			ordinary++
		}
	}
	if len(kept) >= publishJobLimit {
		fail(w, 409, "待核对的发布记录已达上限，请先备份并处理历史记录")
		return
	}
	a.jobs.log.Jobs = append([]publishJob{job}, kept...)
	if err := a.savePublishJobsLocked(); err != nil {
		a.jobs.log.Jobs[0].Status = "failed"
		a.jobs.log.Jobs[0].Message = err.Error()
		fail(w, 503, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(parent, 9*time.Minute)
	done := make(chan struct{})
	a.jobs.cancel, a.jobs.done = cancel, done
	accepted = true
	go func() {
		defer finish()
		a.runPublishJob(ctx, cancel, done, job, service)
	}()
	reply(w, 202, map[string]any{"job": job})
}

func (a *App) updatePublishJob(id string, update func(*publishJob)) error {
	a.jobs.mu.Lock()
	defer a.jobs.mu.Unlock()
	for i := range a.jobs.log.Jobs {
		if a.jobs.log.Jobs[i].ID == id {
			if a.jobs.log.Jobs[i].Status == "interrupted" {
				return errAppStopping
			}
			update(&a.jobs.log.Jobs[i])
			a.jobs.log.Jobs[i].UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			if err := a.savePublishJobsLocked(); err != nil {
				a.jobs.log.Jobs[i].Status = "needs_review"
				a.jobs.log.Jobs[i].Message = err.Error()
				return err
			}
			return nil
		}
	}
	return errors.New("发布记录不存在，已停止操作")
}

func (a *App) runPublishJob(ctx context.Context, cancel context.CancelFunc, done chan struct{}, job publishJob, service model.Service) {
	defer close(done)
	defer cancel()
	defer a.changeMu.Unlock()
	defer func() {
		if recovered := recover(); recovered != nil {
			_ = a.updatePublishJob(job.ID, func(j *publishJob) {
				j.Status, j.Message = "needs_review", "发布执行意外中断，请核对最后一步的实际状态"
			})
		}
	}()
	report := a.publishJobReporter(ctx, job.ID)
	local := publishStep{Kind: "local", Role: "本机配置"}
	err := report(local, "running")
	if err == nil {
		err = func() error {
			if !lockContext(ctx, &a.mu) {
				return ctx.Err()
			}
			defer a.mu.Unlock()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return a.applyLocked(ctx)
		}()
		if err != nil {
			local.Message = err.Error()
			status := "failed"
			if errors.Is(err, errApplyNeedsReview) {
				status = "unknown"
			}
			_ = report(local, status)
		} else {
			local.OK, local.Changes, local.Message = true, true, "本机配置已生效"
			err = report(local, "succeeded")
		}
	}
	if err == nil {
		err = ctx.Err()
		if err == nil {
			_, err = a.publishServiceCloudReported(ctx, service, report)
		}
	}
	a.finishPublishJob(ctx, job.ID, err)
}

func (a *App) publishJobReporter(ctx context.Context, jobID string) publishProgress {
	return func(step publishStep, status string) error {
		// Cancellation never discards a proven result, but it prevents a new
		// local/cloud step from starting after the stop signal.
		if status == "running" && ctx.Err() != nil {
			return ctx.Err()
		}
		return a.updatePublishJob(jobID, func(j *publishJob) {
			// Only allowlisted, bounded presentation fields reach the task file.
			step.Message = truncatePublishText(step.Message)
			step.Steps = nil // EdgeOne plans are not proof that every substep ran.
			entry := publishJobStep{publishStep: step, Status: status, UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
			for i := range j.Steps {
				if j.Steps[i].Kind == step.Kind && j.Steps[i].Host == step.Host {
					j.Steps[i] = entry
					return
				}
			}
			j.Steps = append(j.Steps, entry)
		})
	}
}

func (a *App) finishPublishJob(ctx context.Context, jobID string, err error) {
	_ = a.updatePublishJob(jobID, func(j *publishJob) {
		j.Status, j.Message = "succeeded", "发布配置已完成；外部可达性仍需独立验证"
		failed, succeeded, unknown, waiting := false, false, false, false
		for _, step := range j.Steps {
			if step.Kind == "verification" {
				continue // An access observation is not a cloud publication action.
			}
			succeeded = succeeded || step.Status == "succeeded"
			failed = failed || step.Status == "failed"
			unknown = unknown || step.Status == "unknown" || step.Status == "running"
			waiting = waiting || step.Status == "waiting"
		}
		for _, step := range j.Steps {
			failed = failed || step.Kind != "verification" && step.Status == "skipped" && !waiting
		}
		failed = failed || waiting && j.ContinuationFingerprint == ""
		if waiting && !failed && !unknown && err == nil && j.ContinuationFingerprint != "" {
			schedulePublishWait(j, time.Now())
			return
		}
		j.NextAttemptAt = ""
		if failed || err != nil {
			j.Status, j.Message = "failed", "发布未完成，请处理失败项后重新预览"
			if succeeded {
				j.Status, j.Message = "partial_failed", "部分步骤已完成；已生效的改动未回滚，请查看失败项"
			}
		}
		if unknown || a.jobs.err != nil {
			j.Status, j.Message = "needs_review", "最后一步的结果需要核对；未自动重试，不要直接重复云端写入"
		}
		if ctx.Err() != nil && (err != nil || failed || unknown) && !errors.Is(err, errApplyNeedsReview) {
			j.Status, j.Message = "interrupted", "发布因停止或超时而中断；已确认成功的步骤保留，待核对项未自动重试"
			for i := range j.Steps {
				if j.Steps[i].Status == "running" {
					j.Steps[i].Status = "unknown"
				}
			}
		}
		if err != nil {
			j.Message += "：" + truncatePublishText(err.Error())
		}
		if j.Status == "succeeded" && j.EdgeProvider == "cloudflare" {
			j.Message = "配置与云端解析已更新；DNS 缓存可能尚未刷新，访问状态请单独检测"
		}
	})
}

func truncatePublishText(text string) string {
	const limit = 1200
	runes := []rune(strings.TrimSpace(text))
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return string(runes)
}

func (a *App) stopPublishJob() {
	a.jobs.mu.Lock()
	cancel, done := a.jobs.cancel, a.jobs.done
	a.jobs.mu.Unlock()
	if cancel != nil {
		cancel()
		select {
		case <-done:
		case <-time.After(shutdownTimeout):
			a.interruptPublishJobs()
		}
	}
}

// Called after the drain budget, without the business lock. Preserve completed
// steps and never describe an in-flight write as a confirmed failure.
func (a *App) interruptPublishJobs() {
	a.jobs.mu.Lock()
	defer a.jobs.mu.Unlock()
	changed := false
	for i := range a.jobs.log.Jobs {
		job := &a.jobs.log.Jobs[i]
		if job.Status != "running" {
			continue
		}
		job.Status, job.Message = "interrupted", "管理程序已停止等待发布；请核对本机与云端状态，未自动重试"
		job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		for j := range job.Steps {
			if job.Steps[j].Status == "running" {
				job.Steps[j].Status = "unknown"
			}
		}
		changed = true
	}
	if changed {
		if err := a.savePublishJobsLocked(); err != nil {
			log.Printf("could not persist interrupted publication: %v", err)
		}
	}
}

// A compact list is sufficient for the confirmation screen; it is not another
// model of per-service application or a promise that only one row will change.
func previewImpact(draft, applied model.State, address string) []string {
	impact := []string{}
	for _, item := range previewServices(draft, applied, address) {
		if item.Change != "unchanged" {
			impact = append(impact, fmt.Sprintf("%s（%s）", item.Name, map[string]string{"create": "新增", "update": "修改"}[item.Change]))
		}
	}
	present := map[string]bool{}
	for _, service := range draft.Services {
		present[service.ID] = true
	}
	for _, service := range applied.Services {
		if !present[service.ID] {
			impact = append(impact, service.Name+"（删除）")
		}
	}
	if applied.Settings.GatewayIP != "" && draft.Settings.GatewayIP != applied.Settings.GatewayIP {
		impact = append(impact, "网关监听设置")
	}
	return impact
}
