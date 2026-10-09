package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"w103d/gateway/internal/engine"
	"w103d/gateway/internal/model"
)

// Existing exact record ownership is not a new automatic account choice.
// Resolve only its account, then let the normal record read/plan/write path
// verify the record ID, contents and pending journal before any mutation.
// Never infer ownership for a sibling, parent or validation TXT from this map.
func (a *App) resolveKnownDNSZone(ctx context.Context, host string, refresh bool) (model.DNSAccount, string, string, bool, error) {
	accountID, zone := "", ""
	if binding := a.dnsBindingForHost(host); binding != nil {
		accountID, zone = binding.AccountID, binding.Zone
	} else {
		journal, err := a.readDNSJournal()
		if err != nil {
			return model.DNSAccount{}, "", "", true, errors.New("无法读取 DNS 托管归属，未重新选择解析账号")
		}
		managed, exists := journal.Managed[host]
		if !exists {
			return model.DNSAccount{}, "", "", false, nil
		}
		label, err := labelInZone(host, managed.Zone)
		if err != nil || managed.Record.Key() == "" || managed.Host != "" && managed.Host != host || !strings.EqualFold(managed.Record.Name, label) {
			return model.DNSAccount{}, "", "", true, errors.New("已有 DNS 托管归属不完整或不匹配，请先核对；不会改用其他账号")
		}
		accountID, zone = managed.AccountID, managed.Zone
	}
	account := a.accountByID(accountID)
	if account.ID == "" || !account.Enabled {
		return model.DNSAccount{}, "", "", true, errors.New("原解析账号已停用或不存在，请先核对；不会自动改用其他账号")
	}
	if !engine.ValidDomain(zone) {
		return model.DNSAccount{}, "", "", true, errors.New("已有 DNS 托管域无效，未重新选择解析账号")
	}
	sub, err := labelInZone(host, zone)
	if err != nil {
		return model.DNSAccount{}, "", "", true, err
	}
	if !refresh && time.Since(a.zoneMemoAt) < 30*time.Minute {
		for _, entry := range a.zoneMemo {
			if entry.AccountID == accountID && entry.Zone == zone {
				return account, zone, sub, true, nil
			}
		}
	}
	credentials, err := a.accountCredentials(accountID)
	if err != nil {
		return model.DNSAccount{}, "", "", true, fmt.Errorf("解析账号 %s 的凭据不可用，未改用其他账号：%w", account.Name, err)
	}
	zones, err := a.dnsAPI(credentials).Zones(ctx)
	if err != nil {
		return model.DNSAccount{}, "", "", true, fmt.Errorf("读取所属解析账号 %s 失败，未改用其他账号：%w", account.Name, err)
	}
	for _, found := range zones {
		if strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(found), "."), zone) {
			// Do not promote this one-account answer to a complete discovery cache.
			return account, zone, sub, true, nil
		}
	}
	return model.DNSAccount{}, "", "", true, errors.New("原解析账号已无法管理该托管域，请先核对；不会自动迁移到其他账号")
}
