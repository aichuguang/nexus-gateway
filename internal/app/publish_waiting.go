package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"w103d/gateway/internal/model"
)

const publishWaitLimit = 24 * time.Hour

func publishJobActive(status string) bool { return status == "running" || status == "waiting" }

// Scope continuation to the approved service, its routing settings and account
// identities. Another service's draft or a certificate renewal is not permission
// to change this route, nor a reason to abandon an otherwise valid waiting job.
func publishContinuationFingerprint(state model.State, serviceID string) string {
	var service model.Service
	for _, s := range state.Services {
		if s.ID == serviceID {
			service = s
			break
		}
	}
	if service.ID == "" || !service.Enabled || !service.ServesCloudflare() {
		return ""
	}
	var account model.CloudflareAccount
	for _, a := range state.CloudflareAccounts {
		if a.ID == service.EdgeAccountID {
			account = a
			break
		}
	}
	bindings := []model.DNSBinding{}
	for _, b := range state.DNSBindings {
		names := append([]string{state.Settings.BaseDomain}, service.Domains...)
		for _, host := range names {
			if b.Host == host || strings.HasSuffix(host, "."+b.Host) || strings.HasSuffix(b.Host, "."+host) {
				bindings = append(bindings, b)
				break
			}
		}
	}
	data, _ := json.Marshal(struct {
		Service    model.Service
		Settings   model.Settings
		Cloudflare model.CloudflareAccount
		DNS        []model.DNSAccount
		Bindings   []model.DNSBinding
	}{service, state.Settings, account, state.DNSAccounts, bindings})
	return digest(string(data))
}

func schedulePublishWait(job *publishJob, now time.Time) {
	job.Status = "waiting"
	job.Message = "正在等待 Cloudflare 验证或证书生效，系统会自动继续发布，无需操作"
	for _, step := range job.Steps {
		if step.Status == "waiting" && step.Message != "" {
			job.Message = step.Message
			break
		}
	}
	if job.WaitStartedAt == "" {
		job.WaitStartedAt = now.UTC().Format(time.RFC3339)
	}
	delay := 30 * time.Second
	for i := 0; !cfVerificationOnlyWait(*job) && i < job.Attempts && delay < 5*time.Minute; i++ {
		delay *= 2
	}
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	job.NextAttemptAt = now.Add(delay).UTC().Format(time.RFC3339)
}

// The cloud/DNS work is complete; only read-only access proof remains. Retain
// successful provisioning steps and do not replay their reconciliation.
func cfVerificationOnlyWait(job publishJob) bool {
	if job.EdgeProvider != "cloudflare" {
		return false
	}
	waiting := false
	cfHosts, dnsHosts := map[string]bool{}, map[string]bool{}
	for _, step := range job.Steps {
		if step.Kind == "verification" && step.Status == "waiting" {
			waiting = true
			continue
		}
		if step.Status != "succeeded" || !step.OK || step.RetryProvisioning {
			return false
		}
		if step.Kind == "cloudflare" {
			cfHosts[step.Host] = true
		}
		if step.Kind == "dns" {
			dnsHosts[step.Host] = true
		}
	}
	for _, step := range job.Steps {
		if step.Kind == "verification" && (!cfHosts[step.Host] || !dnsHosts[step.Host]) {
			return false
		}
	}
	return waiting
}

// The scheduler owns no lock while sleeping. Only durable waiting jobs are
// resumable: interrupted or uncertain writes must never enter this queue.
func (a *App) startPublishContinuation() {
	parent, finish, accepted := a.beginWork()
	if !accepted {
		return
	}
	go func() {
		defer finish()
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-parent.Done():
				return
			case now := <-ticker.C:
				ctx, cancel := context.WithTimeout(parent, 90*time.Second)
				a.continuePublishWaiting(ctx, now)
				cancel()
			}
		}
	}()
}

func (a *App) continuePublishWaiting(ctx context.Context, now time.Time) {
	if a.opts.DryRun || ctx.Err() != nil || !a.changeMu.TryLock() {
		return
	}
	defer a.changeMu.Unlock()
	a.jobs.mu.Lock()
	a.loadPublishJobsLocked()
	var job publishJob
	if a.jobs.err == nil {
		for _, candidate := range a.jobs.log.Jobs {
			if candidate.Status != "waiting" {
				continue
			}
			due, err := time.Parse(time.RFC3339, candidate.NextAttemptAt)
			if cfVerificationOnlyWait(candidate) {
				// Honor the shorter read-only cadence for jobs persisted by older
				// releases too, without rewriting their verification result.
				if updated, e := time.Parse(time.RFC3339, candidate.UpdatedAt); e == nil && (err != nil || updated.Add(30*time.Second).Before(due)) {
					due, err = updated.Add(30*time.Second), nil
				}
			}
			if err != nil || !now.Before(due) {
				job = candidate
				break
			}
		}
	}
	a.jobs.mu.Unlock()
	if job.ID == "" {
		return
	}
	defer func() {
		if recover() != nil {
			_ = a.updatePublishJob(job.ID, func(j *publishJob) {
				j.Status, j.Message, j.NextAttemptAt = "needs_review", "自动发布意外中断，请核对最后一步的实际状态", ""
			})
		}
	}()
	stop := func(message string) {
		_ = a.updatePublishJob(job.ID, func(j *publishJob) {
			j.Status, j.Message, j.NextAttemptAt = "partial_failed", message, ""
		})
	}
	started, timeErr := time.Parse(time.RFC3339, job.WaitStartedAt)
	if timeErr != nil || started.After(now.Add(time.Minute)) || now.Sub(started) >= publishWaitLimit {
		stop("等待 Cloudflare 生效已超时或等待记录无效，已停止自动发布；请核对验证、证书、回源及当前业务解析，已提交的 DNS 不会自动回滚")
		return
	}
	if !lockContext(ctx, &a.mu) {
		return
	}
	index := a.serviceIndex(job.ServiceID)
	valid := index >= 0 && job.ContinuationFingerprint != "" && job.ContinuationFingerprint == publishContinuationFingerprint(a.state, job.ServiceID)
	var service model.Service
	if valid {
		service = clone(a.state).Services[index]
	}
	a.mu.Unlock()
	if !valid {
		stop("服务、接入账号或解析配置已变更，旧发布已停止；请按新配置重新发布")
		return
	}
	report := a.publishJobReporter(ctx, job.ID)
	if cfVerificationOnlyWait(job) {
		err := finishLegacyCFAccessWait(ctx, service, report)
		if ctx.Err() == nil {
			a.finishPublishJob(ctx, job.ID, err)
		}
		return
	}
	retryProvisioning := map[string]bool{}
	for _, step := range job.Steps {
		if step.Kind == "cloudflare" && step.Status == "waiting" && step.RetryProvisioning {
			retryProvisioning[step.Host] = true
		}
	}
	resume, err := a.checkCloudflareWaiting(ctx, service, report, retryProvisioning)
	if ctx.Err() != nil || errors.Is(err, errCloudflareWaitRead) {
		_ = a.updatePublishJob(job.ID, func(j *publishJob) {
			j.Attempts++
			schedulePublishWait(j, now)
			j.Message = "暂时无法读取边缘或 DNS 服务状态，系统将自动重查；未重复提交云端写入"
		})
		return
	}
	if err != nil {
		stop(truncatePublishText(err.Error()))
		return
	}
	if !resume {
		_ = a.updatePublishJob(job.ID, func(j *publishJob) { j.Attempts++; schedulePublishWait(j, now) })
		return
	}
	workerContext, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	a.jobs.mu.Lock()
	a.jobs.cancel, a.jobs.done = cancel, done
	a.jobs.mu.Unlock()
	defer close(done)
	defer cancel()
	report = a.publishJobReporter(workerContext, job.ID)
	if err := a.updatePublishJob(job.ID, func(j *publishJob) {
		j.Status, j.Message, j.NextAttemptAt = "running", "正在自动继续接入并发布业务解析", ""
		j.Attempts++
	}); err != nil {
		return
	}
	// Persist running before any possible mutation. A crash from here is not a
	// safe wait and startup must preserve the normal interrupted-write protection.
	_, err = a.publishServiceCloudReported(workerContext, service, report)
	a.finishPublishJob(workerContext, job.ID, err)
}

// Older jobs may have completed all writes and be waiting only for an access
// probe. End that wait without changing their DNS or claiming a probe passed.
// The caller first checks the service fingerprint and all cloud/DNS receipts.
func finishLegacyCFAccessWait(ctx context.Context, s model.Service, report publishProgress) error {
	for _, host := range s.Domains {
		if err := ctx.Err(); err != nil {
			return err
		}
		step := publishStep{Kind: "verification", Host: host, Role: "独立访问检测", OK: false,
			Message: "访问检测不再作为发布条件；此次未检测，不影响已完成的配置与解析更新"}
		if err := reportPublish(report, step, "skipped"); err != nil {
			return err
		}
	}
	return nil
}
