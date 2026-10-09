package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"w103d/gateway/internal/cloudflare"
	"w103d/gateway/internal/model"
)

type cfSSLWriter interface {
	SetSSLMode(context.Context, string, string) (string, error)
}

var errCFProvisioningRead = errors.New("Cloudflare provisioning preflight read temporarily unavailable")

// Consent belongs to the account, not a read-only preview. Journal before the
// single PATCH and reconcile ambiguous results by GET; never blindly retry it.
func (a *App) cfEnsureStrictLocked(ctx context.Context, api cloudflareAPI, account model.CloudflareAccount, act bool) (bool, error) {
	mode, err := cfReadSSLMode(ctx, api, account.ZoneID)
	if err != nil {
		// This GET precedes every mutation in reconciliation. Do not classify
		// write or read-back errors below as safe provisioning retries.
		if cfRetryableRead(err) {
			return false, fmt.Errorf("%w: %w", errCFProvisioningRead, err)
		}
		return false, err
	}
	ledger, err := a.readCloudflareLedger()
	if err != nil {
		return false, err
	}
	key := account.ID + "/ssl-strict"
	previous := ledger.Resources[key]
	if mode == "strict" {
		if act && previous.Pending {
			previous.Pending = false
			ledger.Resources[key] = previous
			if err := a.saveCloudflareLedger(ledger); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	if !account.AutoSSLStrict {
		return false, errors.New("接入区需要 Full (strict)，请在边缘账号中授权自动设置，无需到 Cloudflare 控制台手工修改")
	}
	if previous.Pending {
		return false, errors.New("上次 SSL 模式设置结果仍待核对，本次只读检查，禁止重复写入")
	}
	if !act {
		return true, nil
	}
	if a.opts.DryRun {
		return false, errors.New("dry-run 模式禁止设置 SSL")
	}
	writer, ok := api.(cfSSLWriter)
	if !ok {
		return false, errors.New("当前 Cloudflare 接口不支持自动设置 SSL")
	}
	entry := cloudflareResource{Kind: "ssl", ZoneID: account.ZoneID, ID: account.ZoneID, Value: "strict", BeforeValue: mode, Pending: true, StartedAt: time.Now().UTC().Format(time.RFC3339)}
	ledger.Resources[key] = entry
	if err := a.saveCloudflareLedger(ledger); err != nil {
		return false, err
	}
	value, err := writer.SetSSLMode(ctx, account.ZoneID, "strict")
	if err != nil {
		if cloudflare.Rejected(err) {
			entry.Pending = false
			ledger.Resources[key] = entry
			if e := a.saveCloudflareLedger(ledger); e != nil {
				return true, e
			}
		}
		return true, errors.New("无法完成自动 SSL 设置，请检查 Zone Settings 编辑权限；未知写入不会自动重试")
	}
	if value != "strict" {
		return true, errors.New("SSL 写入响应不匹配，结果待核对，禁止重复写入")
	}
	mode, err = api.SSLMode(ctx, account.ZoneID)
	if err != nil || mode != "strict" {
		return true, errors.New("SSL 设置已提交但尚未核实，结果待核对，禁止重复写入")
	}
	entry.Pending = false
	ledger.Resources[key] = entry
	return true, a.saveCloudflareLedger(ledger)
}
