package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"w103d/gateway/internal/model"
	"w103d/gateway/internal/tencentcloud"
)

// Publishing one service used to be three errands on three pages: apply the
// draft on one, submit each DNS record on another, then run the EdgeOne steps on
// a third. Each of those pages asked for its own confirmation, and each
// confirmation said the same thing — "this is what you just configured" — so the
// operator learned to click through all of them. That habit is the point: a
// confirmation nobody reads is worse than none, because it is still there on the
// day it matters and by then it has been trained away.
//
// This file is the one action those three errands were always part of.
//
// The ordering lives here, in Go, rather than in the .vue file that calls it, for
// the reason the DNS pass already documents: the order is part of the
// requirement, and a rule kept in two places becomes two rules. A second copy in
// the browser would also be untestable by this project's own regime — the smoke
// script drives HTTP and executes no JavaScript at all, which is exactly how a
// publish button stayed broken from 0.3.6 to 0.3.11 without a single test going
// red.
//
// The background job in publish_jobs.go applies the draft before calling this
// cloud half. The legacy synchronous route remains cloud-only for existing
// callers that explicitly apply first; the UI no longer orchestrates two writes.

// publishStep is one thing that was attempted, in the order it was attempted.
type publishStep struct {
	// Kind is the system the step belongs to: "local", "edgeone" or "dns".
	Kind string `json:"kind"`
	Host string `json:"host"`
	// Role names what the name is for, so a result list can say 基站域名 rather
	// than leaving the operator to recognise the name.
	Role string `json:"role,omitempty"`
	// Action is what was done — an EdgeOne step summary, or one of the four DNS
	// actions. Empty when nothing was attempted.
	Action string `json:"action,omitempty"`
	OK     bool   `json:"ok"`
	// Changes separates "ran and changed something" from "ran and found nothing
	// to do". A result list that reports both as success leaves the operator
	// unable to tell a publish that worked from one that was already done — and
	// those are the two cases where the next thing to do differs.
	Changes bool   `json:"changes"`
	Message string `json:"message,omitempty"`
	// Only a proven preflight read failure before this host's first
	// reconciliation may resume its originally authorized provisioning.
	RetryProvisioning bool `json:"retryProvisioning,omitempty"`
	// Steps is the EdgeOne plan's own step list, carried through unchanged so the
	// operator reads what actually ran rather than this program's summary of it.
	Steps []edgeStep `json:"steps,omitempty"`
}

// publishServiceCloud brings one service's cloud footprint in line with what the
// gateway has just been made to serve, and reports every step in order.
//
// It returns steps rather than an error, including when a step fails. A publish
// that stops halfway is not a failure with nothing to say: the operator needs to
// know which names were written before the stop, because those are now live and
// the ones after them are not.
type publishProgress func(publishStep, string) error

func reportPublish(report publishProgress, step publishStep, status string) error {
	if report != nil {
		return report(step, status)
	}
	return nil
}

func (a *App) publishServiceCloud(ctx context.Context, service model.Service) []publishStep {
	steps, _ := a.publishServiceCloudReported(ctx, service, nil)
	return steps
}

func (a *App) publishServiceCloudReported(ctx context.Context, service model.Service, report publishProgress) ([]publishStep, error) {
	if service.ServesCloudflare() && service.Enabled {
		baseSteps, err := a.publishNamesForService(ctx, model.Service{}, nil, report)
		if err != nil {
			return baseSteps, err
		}
		for _, step := range baseSteps {
			if !step.OK {
				return baseSteps, nil
			}
		}
		cfSteps, ready, err := a.publishCloudflareForService(ctx, service, report)
		steps := append(baseSteps, cfSteps...)
		if err != nil {
			return steps, err
		}
		names, err := a.publishNamesForService(ctx, service, ready, report)
		steps = append(steps, names...)
		if err != nil {
			return steps, err
		}
		for _, name := range names {
			if !name.OK {
				return steps, nil
			}
		}
		if len(ready) != len(service.Domains) {
			return steps, nil
		}
		// Completing the DNS write is the publication boundary. Availability is
		// checked separately and must not turn a completed switch into waiting.
		return steps, nil
	}
	steps, edgeReady, err := a.publishEdgeOneForService(ctx, service, report)
	if err != nil {
		return steps, err
	}
	names, err := a.publishNamesForService(ctx, service, edgeReady, report)
	return append(steps, names...), err
}

// publishEdgeOneForService runs the EdgeOne steps for the names this service
// fronts, and reports which of them are now ready to have a DNS record written.
//
// EdgeOne goes first because an EdgeOne-fronted name's record says EdgeOne's
// assigned CNAME — a value that does not exist until the site does. Asking for
// the record first does not merely fail: with no target assigned the gateway
// concludes it does not own the name and *releases* it, which is the handover
// performed backwards.
//
// It takes edgeMu on its own rather than nesting inside the DNS pass. The two
// have always held separate locks — an EdgeOne reconciliation makes several
// cloud calls over tens of seconds, and holding the DNS lock for that long would
// stall the automatic pass — and nesting them here would invent a lock order
// this program has never had. Running one pass after the other is enough,
// because what has to be ordered is the work, not the locking.
func (a *App) publishEdgeOneForService(ctx context.Context, service model.Service, report publishProgress) ([]publishStep, map[string]bool, error) {
	ready := map[string]bool{}
	if !service.ServesEdgeOne() || !service.Enabled {
		return nil, ready, nil
	}
	steps := []publishStep{}
	a.edgeMu.Lock()
	defer a.edgeMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, domain := range service.Domains {
		if err := ctx.Err(); err != nil {
			return steps, ready, err
		}
		host := strings.ToLower(strings.TrimSpace(domain))
		step := publishStep{Kind: "edgeone", Host: host, Role: "加速域名"}
		if err := reportPublish(report, step, "running"); err != nil {
			return steps, ready, err
		}
		status := "failed"
		if a.opts.DryRun {
			step.Message = "开发 dry-run 模式禁止任何 EdgeOne 写入。"
		} else {
			plan, err := a.edgeReconcileLocked(ctx, host, false, true)
			step.Steps = plan.Steps
			if err != nil {
				step.Message = err.Error()
				// A plan is appended before a write. Without a proven rejection,
				// a failed write/read-back may already have changed the cloud.
				if len(plan.Steps) > 0 && !tencentcloud.Rejected(err) {
					status = "unknown"
				}
			} else {
				step.OK, step.Changes, status = true, len(plan.Steps) > 0, "succeeded"
				if step.Changes {
					step.Action = strings.Join(plan.actions(), "、")
					step.Message = fmt.Sprintf("已按 %d 个步骤接入", len(plan.Steps))
				} else {
					step.Message = "已是最新，无需改动"
				}
				ready[host] = true
			}
		}
		steps = append(steps, step)
		if err := reportPublish(report, step, status); err != nil {
			return steps, ready, err
		}
		if status == "unknown" && report != nil {
			return steps, ready, fmt.Errorf("EdgeOne 写入结果待核对，已停止后续发布")
		}
	}
	return steps, ready, nil
}

// publishNamesForService brings this service's DNS names — and the base station
// they lean on — in line with what is being served.
//
// It goes through the same per-name path the automatic pass uses rather than a
// second implementation of it, so the ledger, the write-ahead attempt record and
// the refusal translation are the same ones the operator has already seen. What
// this adds is the selection: this service's names, plus the base station, which
// is included because every directly published name points at it. Publishing a
// service while the name it leans on resolves to nothing is not a publish, and
// the failure would have no symptom on any page that only reads local state.
func (a *App) publishNamesForService(ctx context.Context, service model.Service, edgeReady map[string]bool, report publishProgress) ([]publishStep, error) {
	a.dnsMu.Lock()
	defer a.dnsMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.opts.DryRun {
		step := publishStep{Kind: "dns", Message: "开发 dry-run 模式禁止提交 DNS 写入。"}
		return []publishStep{step}, reportPublish(report, step, "failed")
	}
	journal, err := a.readDNSJournal()
	if err != nil {
		step := publishStep{Kind: "dns", Message: "无法读取本地托管记录，已停止云端操作"}
		return []publishStep{step}, reportPublish(report, step, "failed")
	}
	base := a.baseDomain()
	role := map[string]string{}
	if base != "" {
		role[base] = "基站域名"
	}
	for _, domain := range service.Domains {
		host := strings.ToLower(strings.TrimSpace(domain))
		if host != base {
			role[host] = "服务域名"
		}
	}
	hosts, _ := a.dnsHostsLocked(journal)
	selected := []string{}
	for _, host := range hosts {
		if _, ok := role[host]; ok {
			selected = append(selected, host)
		}
	}
	selected = baseFirst(selected, base)
	steps := []publishStep{}
	for _, host := range selected {
		if err := ctx.Err(); err != nil {
			return steps, err
		}
		step := publishStep{Kind: "dns", Host: host, Role: role[host]}
		if service.Enabled && role[host] == "服务域名" && service.ServesEdge() && !edgeReady[host] {
			step.Message = "等待边缘接入就绪，业务 DNS 尚未切换。"
			if service.ServesEdgeOne() {
				step.Message = "EdgeOne 接入尚未就绪，已保留当前 DNS 记录；完成接入后可继续发布。"
			}
			steps = append(steps, step)
			if err := reportPublish(report, step, "skipped"); err != nil {
				return steps, err
			}
			continue
		}
		if err := reportPublish(report, step, "running"); err != nil {
			return steps, err
		}
		action, changed, err := a.convergeHostLocked(ctx, host)
		step.Action, step.Changes = action, changed
		status := "succeeded"
		if err != nil {
			step.Message, status = err.Error(), "failed"
			current, readErr := a.readDNSJournal()
			if readErr != nil || dnsUnknownForHost(current, host) {
				status = "unknown"
			}
		} else {
			step.OK = true
			if changed {
				step.Message = "已写入（" + action + "）"
			} else {
				step.Message = "已是最新，无需写入"
			}
		}
		steps = append(steps, step)
		if reportErr := reportPublish(report, step, status); reportErr != nil {
			return steps, reportErr
		}
		// A failed base still blocks its dependants. A business-name failure is
		// scoped to that name, so independent names can complete in this pass.
		if err != nil && host == base {
			return steps, nil
		}
	}
	return steps, nil
}

// publishService is the HTTP half of publishServiceCloud: it finds the service,
// runs its cloud steps and answers with the result list.
//
// It deliberately does not apply the draft. Everything it writes is derived from
// what is being served, so applying first is not a convention the caller may
// forget — it is the thing that makes the values correct. What this handler does
// own is the deadline.
func (a *App) publishService(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	index := a.serviceIndex(r.PathValue("id"))
	service := model.Service{}
	if index >= 0 {
		service = a.state.Services[index]
	}
	a.mu.Unlock()
	if index < 0 {
		fail(w, 404, "服务不存在")
		return
	}
	if blocked, err := a.serviceDeletionBlocked(service.ID); err != nil {
		fail(w, 409, err.Error())
		return
	} else if blocked {
		fail(w, 409, "该服务已有删除保护记录，请先保持原选项完成删除，不能重新发布")
		return
	}
	// This route makes several cloud calls in one request — an EdgeOne
	// reconciliation per name, then a read and a write per record — so it can
	// legitimately outlast the server's 45-second write deadline. The deadline is
	// extended for this response alone rather than raised on the server, because
	// the shorter one is right for every other route: it is what stops a stalled
	// client from holding a connection open indefinitely.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Minute))
	ctx, cancel := context.WithTimeout(r.Context(), 9*time.Minute)
	defer cancel()
	reply(w, 200, map[string]any{
		"serviceId": service.ID,
		"service":   service.Name,
		"steps":     a.publishServiceCloud(ctx, service),
	})
}
