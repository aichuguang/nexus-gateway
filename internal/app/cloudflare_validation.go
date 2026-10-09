package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"sort"
	"time"

	"w103d/gateway/internal/cloudflare"
	"w103d/gateway/internal/model"
)

type cfValidationRefresh struct {
	HostnameID string `json:"hostnameId"`
	Challenge  string `json:"challenge"`
	Status     string `json:"status"`
	StartedAt  string `json:"startedAt"`
}

type cfDCVRefresher interface {
	RevalidateHost(context.Context, string, string, cloudflare.SSL) (cloudflare.Hostname, error)
}

// Cloudflare's independent DCV backoff can be hours even while we poll every
// five minutes. Once TXT is DNS-visible, request one immediate recheck per
// challenge. The persisted intent prevents a timeout/restart from replaying it.
func (a *App) refreshCloudflareValidation(ctx context.Context, api cloudflareAPI, account model.CloudflareAccount, h *cloudflare.Hostname, ledger *cloudflareLedger, act bool) (*cloudflare.Hostname, bool, string, error) {
	refresher, ok := api.(cfDCVRefresher)
	if !ok || h == nil || h.SSL.Method != "txt" || h.SSL.Type != "dv" || (h.SSL.Status != "pending_validation" && h.SSL.Status != "validation_timed_out") {
		return h, false, "", nil
	}
	if slices.ContainsFunc(h.SSL.ValidationRecords, func(v cloudflare.ValidationRecord) bool { return v.HTTPURL != "" || v.HTTPBody != "" }) {
		// CF can publish HTTP challenges before its top-level method catches up.
		// Do not replay a stale TXT method during that automatic transition.
		return h, false, "", nil
	}
	if len(h.SSL.ValidationRecords) == 0 {
		return h, false, "等待 Cloudflare 返回证书验证 TXT 信息，尚不能确认验证条件齐备", nil
	}
	values := append([]cloudflare.ValidationRecord{}, h.SSL.ValidationRecords...)
	sort.Slice(values, func(i, j int) bool {
		if values[i].TXTName == values[j].TXTName {
			return values[i].TXTValue < values[j].TXTValue
		}
		return values[i].TXTName < values[j].TXTName
	})
	encoded, _ := json.Marshal(struct {
		CertificateID string
		Values        []cloudflare.ValidationRecord
	}{h.SSL.ID, values})
	challenge := digest(string(encoded))
	key := account.ID + "/" + cfDomain(h.Hostname)
	previous, exists := ledger.ValidationRefreshes[key]
	if exists && previous.HostnameID == h.ID && previous.Challenge == challenge && previous.Status != "rejected" {
		if previous.Status == "pending" || previous.Status == "unknown" {
			return h, false, "证书复验响应待确认，系统正在只读跟踪，不会重复提交", nil
		}
		return h, false, "已请求 Cloudflare 重新验证证书，等待云端处理", nil
	}
	lookup := a.cloudflareTXTLookup
	if lookup == nil {
		lookup = net.DefaultResolver.LookupTXT
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	seen := map[string][]string{}
	for _, v := range values {
		answer, found := seen[v.TXTName]
		if !found {
			var err error
			answer, err = lookup(lookupCtx, v.TXTName)
			if err != nil {
				return h, false, "暂时无法核对证书验证 TXT 的 DNS 可见性，尚未请求 Cloudflare 复验", nil
			}
			seen[v.TXTName] = answer
		}
		if !slices.Contains(answer, v.TXTValue) {
			return h, false, "证书验证 TXT 尚未在 DNS 查询中匹配，正在核对记录与解析传播", nil
		}
	}
	if !act {
		return h, true, "验证 TXT 已在 DNS 中可见，等待 Cloudflare 证书复验", nil
	}
	if a.opts.DryRun {
		return h, false, "", errors.New("dry-run 模式禁止提交证书复验")
	}
	if ledger.ValidationRefreshes == nil {
		ledger.ValidationRefreshes = map[string]cfValidationRefresh{}
	}
	entry := cfValidationRefresh{HostnameID: h.ID, Challenge: challenge, Status: "pending", StartedAt: time.Now().UTC().Format(time.RFC3339)}
	ledger.ValidationRefreshes[key] = entry
	if err := a.saveCloudflareLedger(*ledger); err != nil {
		return h, false, "", err
	}
	result, err := refresher.RevalidateHost(ctx, account.ZoneID, h.ID, h.SSL)
	entry.Status = "submitted"
	if err != nil || result.ID != h.ID || cfDomain(result.Hostname) != cfDomain(h.Hostname) || result.CustomOriginServer != h.CustomOriginServer || result.CustomOriginSNI != h.CustomOriginSNI {
		entry.Status = "unknown"
		if cloudflare.Rejected(err) {
			entry.Status = "rejected"
		}
	}
	ledger.ValidationRefreshes[key] = entry
	if saveErr := a.saveCloudflareLedger(*ledger); saveErr != nil {
		return h, false, "", saveErr
	}
	if entry.Status == "rejected" {
		return h, false, "", fmt.Errorf("Cloudflare 拒绝证书复验：%w", err)
	}
	if entry.Status == "unknown" {
		return h, false, "验证 TXT 已在 DNS 中可见；复验响应待确认，系统会只读跟踪，不会重复提交", nil
	}
	return &result, false, "验证 TXT 已在 DNS 中可见，已通知 Cloudflare 重新验证证书", nil
}
