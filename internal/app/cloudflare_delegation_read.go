package app

import (
	"context"
	"errors"
	"strings"

	"w103d/gateway/internal/cloudflare"
	"w103d/gateway/internal/dnspod"
)

func cfValidDelegationTarget(host, name, target string) bool {
	host, name, target = cfDomain(host), cfDomain(name), cfDomain(target)
	return validCFDomain(host) && name == "_acme-challenge."+host && validCFDomain(target) &&
		strings.HasPrefix(target, host+".") && strings.HasSuffix(target, ".dcv.cloudflare.com")
}

// Existing delegation is observation only. It never grants ownership of the
// CNAME or changes DNS; only the separate confirmed conversion may do that.
func (a *App) cfDelegatedValidationLocked(ctx context.Context, host, name string, records []cloudflare.DCVDelegationRecord) (bool, error) {
	host, name = cfDomain(host), cfDomain(name)
	if name != "_acme-challenge."+host {
		return false, nil
	}
	target := ""
	for _, record := range records {
		if !cfValidDelegationTarget(host, record.CNAME, record.CNAMETarget) {
			return false, errors.New("CF 返回的证书委派记录不属于当前业务域名，未复用或修改 DNS")
		}
		candidate := cfDomain(record.CNAMETarget)
		if target != "" && candidate != target {
			return false, errors.New("CF 返回多个不同证书委派目标，未复用或修改 DNS")
		}
		target = candidate
	}
	account, zone, sub, err := a.resolveZone(ctx, name, false)
	if err != nil {
		return false, err
	}
	credentials, err := a.accountCredentials(account.ID)
	if err != nil {
		return false, err
	}
	existing, err := a.dnsAPI(credentials).List(ctx, zone, sub)
	if err != nil {
		return false, err
	}
	for _, record := range existing {
		if !strings.EqualFold(record.Type, dnspod.TypeCNAME) {
			continue
		}
		if len(existing) != 1 || target == "" || !strings.EqualFold(record.Name, sub) ||
			cfDomain(record.Value) != target || record.Status != "ENABLE" || record.Proxied ||
			record.Line != dnspod.DefaultLine || record.LineID != "0" || record.Weight != 0 {
			return false, errors.New("证书验证名称的 CNAME 与当前 CF 委派不匹配或存在共存冲突，未覆盖现有记录")
		}
		return true, nil
	}
	return false, nil
}
