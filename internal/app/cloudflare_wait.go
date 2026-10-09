package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"w103d/gateway/internal/cloudflare"
	"w103d/gateway/internal/dnsprovider"
	"w103d/gateway/internal/model"
)

var errCloudflareWaitRead = errors.New("Cloudflare status temporarily unavailable")

func cfRetryableRead(err error) bool {
	var apiErr *cloudflare.APIError
	var netErr net.Error
	return dnsprovider.RetryableRead(err) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		errors.As(err, &netErr) || errors.As(err, &apiErr) && (apiErr.Status == 429 || apiErr.Status >= 500 || apiErr.Status == 408) ||
		strings.Contains(err.Error(), "Cloudflare transport failed")
}

// Provider error bodies stay private. Translate recognizable permanent causes
// into bounded messages rather than exposing arbitrary remote text or tokens.
// Missing TXT/DNS observations during propagation are not terminal failures.
func cfProvisioningFailure(h *cloudflare.Hostname, fallback cloudflare.Fallback) error {
	terminal := func(status string) bool {
		switch strings.ToLower(status) {
		case "failed", "blocked", "expired", "revoked", "deleted", "deleting", "pending_deletion", "moved":
			return true
		}
		return false
	}
	if terminal(fallback.Status) {
		return errors.New("Cloudflare 回源配置失败，自动发布已停止；请检查回源域名和源站 HTTPS")
	}
	if h == nil {
		return nil
	}
	issues := append([]string{}, h.VerificationErrors...)
	for _, issue := range h.SSL.ValidationErrors {
		issues = append(issues, issue.Message)
	}
	for _, issue := range issues {
		issue = strings.ToLower(issue)
		if strings.Contains(issue, "caa") && (strings.Contains(issue, "forbid") || strings.Contains(issue, "not permit") || strings.Contains(issue, "not authoriz") || strings.Contains(issue, "restrict") || strings.Contains(issue, "denied") || strings.Contains(issue, "prevent") || strings.Contains(issue, "failed")) {
			return errors.New("Cloudflare 证书签发被 CAA 记录限制，自动发布已停止；需要允许当前签发机构后继续")
		}
		if strings.Contains(issue, "rate limit") || strings.Contains(issue, "quota exceeded") {
			return errors.New("Cloudflare 验证或签发达到服务商限制，自动发布已停止；限制解除后可继续")
		}
	}
	if terminal(h.Status) {
		return errors.New("Cloudflare 主机名验证失败或资源已失效，自动发布已停止；请检查域名接入状态")
	}
	if terminal(h.SSL.Status) {
		return errors.New("Cloudflare 边缘证书签发失败或证书已失效，自动发布已停止；请检查证书验证状态")
	}
	return nil
}

func cfWaitingMessage(h *cloudflare.Hostname, fallback cloudflare.Fallback) string {
	if h == nil {
		return "正在准备 Cloudflare 接入"
	}
	if h.Status != "active" {
		return "正在等待域名验证生效"
	}
	if h.SSL.Status != "active" {
		return "正在等待边缘 SSL 证书生效"
	}
	if fallback.Status != "active" {
		return "正在等待 Cloudflare 回源配置生效"
	}
	return "正在核对 Cloudflare 接入"
}

func cfInitialProvisioningStep(action edgeStep, ledger cloudflareLedger, accountID, host string) bool {
	key := ""
	switch action.Action {
	case "create-hostname":
		key = "host/" + cfDomain(host)
	case "create-fallback-dns":
		key = "fallback-dns"
	case "create-target-dns":
		key = "target-dns"
	case "set-fallback":
		key = "fallback"
	case "set-ssl-strict":
		key = "ssl-strict"
	default:
		return false
	}
	_, provisioned := ledger.Resources[accountID+"/"+key]
	return !provisioned
}

// checkCloudflareWaiting only observes an already-authorized publication. The
// caller validates the persisted service/configuration fingerprint first. It
// must release its write gate between calls, not sleep while holding these locks.
// resume means a hostname needs its authorized initial provisioning, every
// hostname is ready, or validation needs write-ahead protected reconciliation.
// Initial retries cannot recreate recorded resources or replace shared origin.
// No cloud/ledger writes or new DNS readiness grants occur here.
func (a *App) checkCloudflareWaiting(ctx context.Context, s model.Service, report publishProgress, initialRetries ...map[string]bool) (resume bool, err error) {
	if !s.Enabled || !s.ServesCloudflare() {
		return false, errors.New("Cloudflare 发布配置已变化，等待任务不能继续")
	}
	if !lockContext(ctx, &a.cloudflareMu) {
		return false, ctx.Err()
	}
	defer a.cloudflareMu.Unlock()
	if !lockContext(ctx, &a.dnsMu) {
		return false, ctx.Err()
	}
	defer a.dnsMu.Unlock()
	if !lockContext(ctx, &a.mu) {
		return false, ctx.Err()
	}
	defer a.mu.Unlock()
	ledger, err := a.readCloudflareLedger()
	if err != nil {
		return false, err
	}
	for key, resource := range ledger.Resources {
		if strings.HasPrefix(key, s.EdgeAccountID+"/") && resource.Pending {
			return false, errors.New("Cloudflare 存在结果未知的写入，已停止自动续发；请先核对原操作")
		}
	}
	if txt, readErr := a.readDNSTXTJournal(); readErr != nil {
		return false, readErr
	} else if txt.Attempt != nil {
		return false, errors.New("DNS TXT 存在结果未知的写入，已停止自动续发；请先核对原操作")
	}
	allReady, missingTXT, refreshDCV, provisioning := len(s.Domains) > 0, false, false, false
	for _, host := range s.Domains {
		initialRetry := len(initialRetries) > 0 && initialRetries[0][cfDomain(host)]
		// Once any reconciliation reached this host, ordinary wait drift
		// protection applies even if an older receipt still contains the flag.
		if _, exists := ledger.ReadyHosts[s.EdgeAccountID+"/"+cfDomain(host)]; exists {
			initialRetry = false
		}
		if _, exists := ledger.Resources[s.EdgeAccountID+"/host/"+cfDomain(host)]; exists {
			initialRetry = false
		}
		plan, checkErr := a.cfReconcileLocked(ctx, host, false, true)
		if checkErr != nil && cfRetryableRead(checkErr) {
			return false, fmt.Errorf("%w: %v", errCloudflareWaitRead, checkErr)
		}
		if checkErr == nil {
			for _, action := range plan.Steps {
				if action.Action != "ensure-validation-txt" && action.Action != "use-dcv-delegation" && !(initialRetry && cfInitialProvisioningStep(action, ledger, s.EdgeAccountID, host)) {
					return false, errors.New("等待期间接入资源发生变化，已停止自动发布；不会自动重建主机名或替换共享回源")
				}
			}
		}
		step := publishStep{Kind: "cloudflare", Host: cfDomain(host), Role: "CF 加速域名", Message: plan.Message, OK: plan.Ready, RetryProvisioning: initialRetry}
		status := "waiting"
		if checkErr == nil {
			provisioning = provisioning || initialRetry
			allReady = allReady && plan.Ready
			refreshDCV = refreshDCV || plan.DCVRefreshDue
			for _, validation := range plan.ValidationRecords {
				delegated, e := a.cfDelegatedValidationLocked(ctx, host, validation.Name, plan.DCVDelegationRecords)
				if e != nil {
					checkErr = e
					break
				}
				if delegated {
					continue
				}
				present, e := a.cloudflareValidationPresentLocked(ctx, validation)
				if e != nil {
					checkErr = e
					break
				}
				missingTXT = missingTXT || !present
			}
			if plan.Ready {
				status = "succeeded"
			}
		}
		if checkErr != nil {
			if cfRetryableRead(checkErr) {
				return false, fmt.Errorf("%w: %v", errCloudflareWaitRead, checkErr)
			}
			step.OK, step.Message, status = false, checkErr.Error(), "failed"
		}
		if e := reportPublish(report, step, status); e != nil {
			return false, e
		}
		if checkErr != nil {
			return false, checkErr
		}
	}
	return allReady || missingTXT || refreshDCV || provisioning, nil
}

func (a *App) cloudflareValidationPresentLocked(ctx context.Context, validation cfValidation) (bool, error) {
	account, zone, sub, err := a.resolveZone(ctx, validation.Name, false)
	if err != nil {
		return false, err
	}
	credentials, err := a.accountCredentials(account.ID)
	if err != nil {
		return false, err
	}
	records, err := a.dnsAPI(credentials).List(ctx, zone, sub)
	if err != nil {
		return false, err
	}
	return validateTXTRecords(records, validation.Value)
}
