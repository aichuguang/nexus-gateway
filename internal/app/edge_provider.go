package app

import (
	"fmt"
	"w103d/gateway/internal/model"
)

func cloudflareServiceAccount(state model.State, service model.Service) error {
	for _, account := range state.CloudflareAccounts {
		if account.ID == service.EdgeAccountID && account.Enabled {
			for _, host := range service.Domains {
				if host == account.FallbackDomain || host == account.StandardTarget || host == account.ZoneName {
					return fmt.Errorf("业务域名不能占用 Cloudflare 接入区或回源名称")
				}
			}
			if service.UsePreferred && service.PreferredDomain == account.FallbackDomain {
				return fmt.Errorf("优选入口不能指向 Cloudflare 回源名称")
			}
			return nil
		}
	}
	return fmt.Errorf("请选择启用的 Cloudflare 边缘账号")
}

func edgeServiceAccountAvailable(state model.State, service model.Service) error {
	if !service.Enabled || !service.ServesEdge() {
		return nil
	}
	if service.ServesCloudflare() {
		return cloudflareServiceAccount(state, service)
	}
	if service.ServesEdgeOne() {
		for _, account := range state.EdgeOneAccounts {
			if account.Enabled {
				return nil
			}
		}
		return fmt.Errorf("还没有可用的 EdgeOne 账号，请先到边缘加速页面添加账号")
	}
	return fmt.Errorf("不支持的边缘提供商")
}

// Edge settings change publication even though they do not change an Nginx block.
func sameEntryPolicy(a, b model.Service) bool {
	// Preserve the pre-existing direct/EdgeOne handover behavior. Cloudflare's
	// account and preferred target require an explicit publication/check cycle.
	if !a.ServesCloudflare() && !b.ServesCloudflare() {
		return true
	}
	return a.EntryMode == b.EntryMode && a.EffectiveEdgeProvider() == b.EffectiveEdgeProvider() && a.EdgeAccountID == b.EdgeAccountID && a.UsePreferred == b.UsePreferred && a.PreferredDomain == b.PreferredDomain
}
