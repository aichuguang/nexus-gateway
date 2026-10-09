package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"w103d/gateway/internal/cloudflare"
	"w103d/gateway/internal/dnspod"
	"w103d/gateway/internal/dnsprovider"
	"w103d/gateway/internal/model"
)

type serviceDeleteCloudflare struct {
	Host       string   `json:"host"`
	AccountID  string   `json:"accountId"`
	ZoneID     string   `json:"zoneId"`
	HostnameID string   `json:"hostnameId"`
	Cnames     []string `json:"cnames,omitempty"`
	Attempt    bool     `json:"attempt,omitempty"`
	Done       bool     `json:"done,omitempty"`
}

type serviceDeleteTXT struct {
	Key     string     `json:"key"`
	Managed dnsManaged `json:"managed"`
	Attempt bool       `json:"attempt,omitempty"`
	Done    bool       `json:"done,omitempty"`
}

type cfHostnameDeleter interface {
	DeleteHost(context.Context, string, string) error
}

func cfDeletionKey(account, host string) string { return account + "/host/" + host }

func validCFManagedValidation(m dnsManaged) bool {
	if m.Record.Type == dnspod.TypeTXT {
		return m.Host == "_acme-challenge."+m.ValidationHost || m.Host == "_cf-custom-hostname."+m.ValidationHost
	}
	return m.Record.Type == dnspod.TypeCNAME && m.Host == "_acme-challenge."+m.ValidationHost &&
		cfValidDelegationTarget(m.ValidationHost, m.Host, m.Record.Value) &&
		m.Record.Status == "ENABLE" && !m.Record.Proxied && m.Record.Line == dnspod.DefaultLine && m.Record.LineID == "0" && m.Record.Weight == 0
}

func validCloudflareServiceDeletion(entry serviceDeletion, hosts map[string]bool) bool {
	seen := map[string]bool{}
	for _, target := range entry.Cloudflare {
		key := cfDeletionKey(target.AccountID, target.Host)
		if !entry.DeleteCloudflare || !hosts[target.Host] || target.AccountID == "" || !cfZoneIDPattern.MatchString(target.ZoneID) || target.HostnameID == "" || len(target.HostnameID) > 1024 || seen[key] {
			return false
		}
		for _, cname := range target.Cnames {
			if !validCFDomain(cname) {
				return false
			}
		}
		seen[key] = true
	}
	keys := map[string]bool{}
	for _, target := range entry.TXT {
		m := target.Managed
		sum := sha256.Sum256([]byte(m.AccountID + "\x00" + m.Host + "\x00" + m.Record.Value))
		if !entry.DeleteCloudflare || !seen[cfDeletionKey(m.ValidationAccountID, m.ValidationHost)] || !validCFManagedValidation(m) || m.AccountID == "" || m.Zone == "" || m.Record.Key() == "" || target.Key != hex.EncodeToString(sum[:]) || keys[target.Key] {
			return false
		}
		keys[target.Key] = true
	}
	return true
}

// A local creation receipt, exact remote ID and matching host are all required.
// Shared fallback/ingress DNS and zone SSL belong to the account, not a service.
func (a *App) planCloudflareServiceDeletion(ctx context.Context, states []model.State, entry *serviceDeletion) error {
	ledger, err := a.readCloudflareLedger()
	if err != nil {
		return err
	}
	selected := map[string]*serviceDeleteCloudflare{}
	hasCF := false
	for _, state := range states {
		for _, service := range state.Services {
			if service.ID != entry.ServiceID || !service.ServesCloudflare() {
				continue
			}
			hasCF = true
			for _, host := range service.Domains {
				host = cfDomain(host)
				if !slices.Contains(entry.Hosts, host) {
					continue
				}
				index := a.cloudflareAccountIndex(service.EdgeAccountID)
				if index < 0 {
					return errors.New("待清理主机名的 Cloudflare 账号不存在，未执行云端删除")
				}
				account := a.state.CloudflareAccounts[index]
				key := cfDeletionKey(account.ID, host)
				if selected[key] == nil {
					resource, owned := ledger.Resources[key]
					if resource.Pending {
						return errors.New("Cloudflare 主机名创建结果尚未核对，不能直接转为删除")
					}
					credentials, e := a.cloudflareCredentials(account.ID)
					if e != nil {
						return e
					}
					h, e := a.cfAPI(credentials).Host(ctx, account.ZoneID, host)
					if e != nil {
						return e
					}
					if !owned || !resource.Owned || resource.Kind != "hostname" || resource.ID == "" || resource.Value != host {
						if h == nil {
							continue
						}
						return fmt.Errorf("%s 的 CF 创建归属凭据缺失，未执行云端删除；需核对原创建记录，不能只按同名资源删除", host)
					}
					// Older releases recorded Owned + exact ID under the account key,
					// but no ZoneID. The live lookup in this account's zone and the
					// ID check below are sufficient; missing metadata is not foreign
					// ownership. A populated, different zone remains a hard conflict.
					if resource.ZoneID != "" && resource.ZoneID != account.ZoneID {
						return fmt.Errorf("%s 的 CF 创建记录所属区域与当前账号不一致，未执行云端删除", host)
					}
					target := &serviceDeleteCloudflare{Host: host, AccountID: account.ID, ZoneID: account.ZoneID, HostnameID: resource.ID, Done: h == nil}
					if h != nil {
						if e := validateDeleteCloudflare(*target, h); e != nil {
							return e
						}
					}
					selected[key] = target
				}
				target := selected[key]
				if target == nil {
					continue
				}
				for _, candidate := range state.CloudflareAccounts {
					if candidate.ID != account.ID {
						continue
					}
					cname := candidate.StandardTarget
					if service.UsePreferred {
						cname = cfDomain(service.PreferredDomain)
					}
					if validCFDomain(cname) && !slices.Contains(target.Cnames, cname) {
						target.Cnames = append(target.Cnames, cname)
					}
				}
				binding := ledger.ReadyHosts[account.ID+"/"+host]
				if validCFDomain(binding.Target) && !slices.Contains(target.Cnames, binding.Target) {
					target.Cnames = append(target.Cnames, binding.Target)
				}
			}
		}
	}
	if !hasCF {
		return errors.New("该服务没有 Cloudflare 接入配置，不能使用 CF 清理选项")
	}
	keys := make([]string, 0, len(selected))
	for key := range selected {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		entry.Cloudflare = append(entry.Cloudflare, *selected[key])
	}
	txt, err := a.readDNSTXTJournal()
	if err != nil {
		return err
	}
	unknown := func(attempt *dnsTXTAttempt) bool {
		return attempt != nil && selected[cfDeletionKey(attempt.ValidationAccountID, attempt.ValidationHost)] != nil
	}
	if unknown(txt.Attempt) {
		return errors.New("该服务验证 TXT 有未核对的写入，未执行删除")
	}
	for _, attempt := range txt.Attempts {
		if unknown(attempt) {
			return errors.New("该服务验证 TXT 有未核对的写入，未执行删除")
		}
	}
	keys = nil
	for key, m := range txt.Managed {
		if selected[cfDeletionKey(m.ValidationAccountID, m.ValidationHost)] != nil {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		m := txt.Managed[key]
		if !validCFManagedValidation(m) {
			return errors.New("验证记录不属于允许清理的托管 TXT 或 CF 委派 CNAME，未执行删除")
		}
		if pending, err := a.txtCleanupUnknown(key); err != nil || pending {
			return errors.New("验证 TXT 有旧删除结果待核对，未重复操作")
		}
		_, present, err := a.inspectDeleteDNS(ctx, serviceDeleteDNS{Managed: m, Delete: true})
		if err != nil {
			return err
		}
		entry.TXT = append(entry.TXT, serviceDeleteTXT{Key: key, Managed: m, Done: !present})
	}
	return nil
}

func validateDeleteCloudflare(target serviceDeleteCloudflare, h *cloudflare.Hostname) error {
	if h.ID != target.HostnameID || cfDomain(h.Hostname) != target.Host || h.CustomOriginServer != "" || h.CustomOriginSNI != "" {
		return fmt.Errorf("%s 的 CF 主机名 ID 或回源配置已变化，拒绝删除非原托管资源", target.Host)
	}
	return nil
}

func (a *App) executeCloudflareServiceDeletion(ctx context.Context, log serviceDeletionLog, entry *serviceDeletion) error {
	for i := range entry.Cloudflare {
		target := &entry.Cloudflare[i]
		if target.Done {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		index := a.cloudflareAccountIndex(target.AccountID)
		if index < 0 || a.state.CloudflareAccounts[index].ZoneID != target.ZoneID {
			return errors.New("CF 账号或接入区已变化，未继续删除")
		}
		credentials, err := a.cloudflareCredentials(target.AccountID)
		if err != nil {
			return err
		}
		api := a.cfAPI(credentials)
		h, err := api.Host(ctx, target.ZoneID, target.Host)
		if err != nil {
			return err
		}
		if h != nil {
			if err := validateDeleteCloudflare(*target, h); err != nil {
				return err
			}
			if target.Attempt {
				return errors.New("CF 主机名删除结果仍待核对；保留原选项再次核对即可，系统不会重复发送删除")
			}
			deleter, ok := api.(cfHostnameDeleter)
			if !ok {
				return errors.New("当前 CF 接口不支持主机名删除")
			}
			target.Attempt = true
			if err := a.saveServiceDeletions(log); err != nil {
				return err
			}
			if err := deleter.DeleteHost(ctx, target.ZoneID, target.HostnameID); err != nil {
				if cloudflare.Rejected(err) {
					target.Attempt = false
					if e := a.saveServiceDeletions(log); e != nil {
						return e
					}
				}
				return fmt.Errorf("CF 主机名删除尚未确认：%w", err)
			}
			h, err = api.Host(ctx, target.ZoneID, target.Host)
			if err != nil || h != nil {
				return errors.New("CF 已接受主机名删除，尚待确认生效；保留原选项再次核对，不会重复删除")
			}
		}
		target.Done, target.Attempt = true, false
		if err := a.saveServiceDeletions(log); err != nil {
			return err
		}
	}
	for i := range entry.TXT {
		target := &entry.TXT[i]
		if target.Done {
			continue
		}
		client, present, err := a.inspectDeleteDNS(ctx, serviceDeleteDNS{Managed: target.Managed, Delete: true})
		if err != nil {
			return err
		}
		if present {
			if target.Attempt {
				return errors.New("验证 TXT 删除结果待核对，未重复删除")
			}
			if err := a.authorizeDNSWrite(target.Managed.AccountID); err != nil {
				return err
			}
			target.Attempt = true
			if err := a.saveServiceDeletions(log); err != nil {
				return err
			}
			if err := client.Delete(ctx, target.Managed.Zone, target.Managed.Record.Key()); err != nil {
				if dnsprovider.Rejected(err) {
					target.Attempt = false
					if e := a.saveServiceDeletions(log); e != nil {
						return e
					}
				}
				return fmt.Errorf("验证 TXT 删除未确认：%w", err)
			}
			_, remaining, err := a.inspectDeleteDNS(ctx, serviceDeleteDNS{Managed: target.Managed, Delete: true})
			if err != nil || remaining {
				return errors.New("验证 TXT 删除已提交，尚待核对生效，未重复删除")
			}
		}
		target.Done, target.Attempt = true, false
		if err := a.saveServiceDeletions(log); err != nil {
			return err
		}
	}
	if !entry.DeleteCloudflare {
		return nil
	}
	ledger, err := a.readCloudflareLedger()
	if err != nil {
		return err
	}
	for _, target := range entry.Cloudflare {
		key := cfDeletionKey(target.AccountID, target.Host)
		if recorded, ok := ledger.Resources[key]; ok {
			if recorded.ID != target.HostnameID {
				return errors.New("CF 托管记录已变化，未解除其他资源的归属")
			}
			delete(ledger.Resources, key)
		}
		delete(ledger.ReadyHosts, target.AccountID+"/"+target.Host)
		delete(ledger.ValidationRefreshes, target.AccountID+"/"+target.Host)
	}
	if err := a.saveCloudflareLedger(ledger); err != nil {
		return err
	}
	txt, err := a.readDNSTXTJournal()
	if err != nil {
		return err
	}
	for _, target := range entry.TXT {
		if m, ok := txt.Managed[target.Key]; ok && !sameDNSRecord(m.Record, target.Managed.Record) {
			return errors.New("验证 TXT 托管记录已变化，未解除归属")
		}
		delete(txt.Managed, target.Key)
	}
	return a.saveDNSTXTJournal(txt)
}

func (a *App) stopWaitingServicePublication(serviceID string) error {
	a.jobs.mu.Lock()
	defer a.jobs.mu.Unlock()
	a.loadPublishJobsLocked()
	if a.jobs.err != nil {
		return a.jobs.err
	}
	changed := false
	for i := range a.jobs.log.Jobs {
		job := &a.jobs.log.Jobs[i]
		if job.ServiceID != serviceID || job.Status != "waiting" {
			continue
		}
		job.Status, job.Message, job.NextAttemptAt = "partial_failed", "服务已进入删除流程，自动发布已停止", ""
		job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		changed = true
	}
	if changed {
		return a.savePublishJobsLocked()
	}
	return nil
}
