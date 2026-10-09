package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"w103d/gateway/internal/dnspod"
	"w103d/gateway/internal/dnsprovider"
	"w103d/gateway/internal/engine"
	"w103d/gateway/internal/model"
	"w103d/gateway/internal/teo"
)

type serviceDeleteOptions struct {
	DeleteDNS        bool   `json:"deleteDNS"`
	DeleteEdgeOne    bool   `json:"deleteEdgeOne"`
	DeleteCloudflare bool   `json:"deleteCloudflare"`
	Revision         *int64 `json:"revision,omitempty"`
}

type serviceDeleteDNS struct {
	Managed dnsManaged `json:"managed"`
	Delete  bool       `json:"delete"`
	Attempt bool       `json:"attempt,omitempty"`
	Done    bool       `json:"done,omitempty"`
}

type serviceDeleteEdge struct {
	Host      string `json:"host"`
	AccountID string `json:"accountId"`
	ZoneID    string `json:"zoneId"`
	GroupID   string `json:"groupId"`
	Cname     string `json:"cname"`
	Attempt   string `json:"attempt,omitempty"`
	Done      bool   `json:"done,omitempty"`
}

// Deletions are write-ahead authorizations, not a work queue. No background
// worker resumes them. Completed entries also prevent an old applied snapshot
// or a restored history entry from recreating cloud resources behind the user.
type serviceDeletion struct {
	ServiceID        string                    `json:"serviceId"`
	DeleteDNS        bool                      `json:"deleteDNS"`
	DeleteEdgeOne    bool                      `json:"deleteEdgeOne"`
	DeleteCloudflare bool                      `json:"deleteCloudflare,omitempty"`
	Hosts            []string                  `json:"hosts"`
	DNS              []serviceDeleteDNS        `json:"dns"`
	Edge             []serviceDeleteEdge       `json:"edge"`
	Cloudflare       []serviceDeleteCloudflare `json:"cloudflare,omitempty"`
	TXT              []serviceDeleteTXT        `json:"txt,omitempty"`
	Completed        bool                      `json:"completed"`
}

type serviceDeletionLog struct {
	Format  int                         `json:"format"`
	Entries map[string]*serviceDeletion `json:"entries"`
}

func (a *App) serviceDeletionPath() string {
	return filepath.Join(a.opts.DataDir, "service-deletions.json")
}

func (a *App) readServiceDeletions() (serviceDeletionLog, error) {
	log := serviceDeletionLog{Format: 1, Entries: map[string]*serviceDeletion{}}
	info, err := os.Lstat(a.serviceDeletionPath())
	if errors.Is(err, os.ErrNotExist) {
		return log, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return log, errors.New("无法读取服务删除保护记录；已停止云端改动，请检查数据目录")
	}
	data, err := os.ReadFile(a.serviceDeletionPath())
	var loaded serviceDeletionLog
	if err != nil || json.Unmarshal(data, &loaded) != nil || (loaded.Format != 1 && loaded.Format != 2) || loaded.Entries == nil {
		return log, errors.New("服务删除保护记录损坏；已停止云端改动，请检查数据目录")
	}
	log = loaded
	for id, entry := range log.Entries {
		if entry == nil || entry.ServiceID == "" || entry.ServiceID != id || !validServiceDeletion(*entry) {
			return log, errors.New("服务删除保护记录不合法；已停止云端改动")
		}
	}
	return log, nil
}

func validServiceDeletion(entry serviceDeletion) bool {
	hosts := map[string]bool{}
	for _, host := range entry.Hosts {
		if !engine.ValidDomain(host) || !strings.Contains(host, ".") || normalizedDeleteHost(host) != host || hosts[host] {
			return false
		}
		hosts[host] = true
	}
	for _, target := range entry.Edge {
		if !entry.DeleteEdgeOne || !hosts[target.Host] || target.AccountID == "" || target.ZoneID == "" || target.GroupID == "" ||
			target.Attempt != "" && target.Attempt != "offline" && target.Attempt != "delete" {
			return false
		}
	}
	if !validCloudflareServiceDeletion(entry, hosts) {
		return false
	}
	for _, target := range entry.DNS {
		m := target.Managed
		if !hosts[m.Host] || m.AccountID == "" || m.Zone == "" || m.Record.Key() == "" || target.Attempt && !target.Delete {
			return false
		}
		if target.Delete && !entry.DeleteDNS {
			associated := false
			for _, edge := range entry.Edge {
				associated = associated || edge.Host == m.Host && edge.Cname != "" && m.Record.Type == dnspod.TypeCNAME && sameRecordValue(dnspod.TypeCNAME, edge.Cname, m.Record.Value)
			}
			for _, cf := range entry.Cloudflare {
				for _, cname := range cf.Cnames {
					associated = associated || entry.DeleteCloudflare && cf.Host == m.Host && m.Record.Type == dnspod.TypeCNAME && sameRecordValue(dnspod.TypeCNAME, cname, m.Record.Value)
				}
			}
			if (!entry.DeleteEdgeOne && !entry.DeleteCloudflare) || !associated {
				return false
			}
		}
	}
	return true
}

func (a *App) saveServiceDeletions(log serviceDeletionLog) error {
	for _, entry := range log.Entries {
		if entry == nil || !validServiceDeletion(*entry) {
			return errors.New("服务删除目标缺少明确归属，无法保存删除保护记录")
		}
		// Old readers must refuse CF deletion intents rather than silently
		// dropping the new targets and completing only the local deletion.
		if entry.DeleteCloudflare {
			log.Format = 2
		}
	}
	data, err := json.MarshalIndent(log, "", "  ")
	if err != nil || len(data) > 4<<20 {
		return errors.New("服务删除保护记录超过安全容量，未继续操作，请归档数据后重试")
	}
	if err := writeJSON(a.serviceDeletionPath(), log); err != nil {
		return errors.New("无法保存服务删除保护记录；已停止继续操作，请检查数据目录")
	}
	return nil
}

func (a *App) serviceDeletionBlocked(id string) (bool, error) {
	log, err := a.readServiceDeletions()
	return log.Entries[id] != nil, err
}

func (a *App) refuseDeletionProtectedState(state model.State) error {
	log, err := a.readServiceDeletions()
	if err != nil {
		return err
	}
	for _, service := range state.Services {
		if log.Entries[service.ID] != nil {
			return errors.New("有服务处于删除保护状态，请先保持原选项完成删除，再应用或发布配置")
		}
	}
	return nil
}

func (a *App) deletionBlocksHost(host string) (bool, error) {
	log, err := a.readServiceDeletions()
	if err != nil {
		return true, err
	}
	service, claimed := a.serviceForHost(host)
	for _, entry := range log.Entries {
		if claimed && (entry.Completed || a.serviceIndex(entry.ServiceID) < 0) {
			continue
		}
		for _, recorded := range entry.Hosts {
			if recorded == host {
				return true, nil
			}
		}
	}
	if claimed {
		return log.Entries[service.ID] != nil, nil
	}
	return false, nil
}

func decodeServiceDelete(w http.ResponseWriter, r *http.Request, input *serviceDeleteOptions) bool {
	if r.Body == nil {
		return true
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		fail(w, 400, "删除请求格式不正确")
		return false
	}
	if len(strings.TrimSpace(string(data))) == 0 || strings.TrimSpace(string(data)) == "null" {
		return true // Older clients send an empty DELETE: cloud cleanup is opt-in.
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		fail(w, 415, "需要 JSON 请求")
		return false
	}
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	var extra any
	if d.Decode(input) != nil || d.Decode(&extra) != io.EOF {
		fail(w, 400, "删除请求格式不正确")
		return false
	}
	return true
}

func (a *App) deleteService(w http.ResponseWriter, r *http.Request) {
	var input serviceDeleteOptions
	if !decodeServiceDelete(w, r, &input) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	// The changing middleware owns changeMu. Preserve the global lock order;
	// direct read-only previews take these same locks before reading cloud state.
	if !lockContext(ctx, &a.edgeMu) {
		fail(w, 409, "等待 EdgeOne 操作结束超时，未执行删除，请稍后重试")
		return
	}
	defer a.edgeMu.Unlock()
	if !lockContext(ctx, &a.cloudflareMu) {
		fail(w, 409, "等待 Cloudflare 操作结束超时，未执行删除")
		return
	}
	defer a.cloudflareMu.Unlock()
	if !lockContext(ctx, &a.dnsMu) {
		fail(w, 409, "等待 DNS 操作结束超时，未执行删除，请稍后重试")
		return
	}
	defer a.dnsMu.Unlock()
	if !lockContext(ctx, &a.mu) {
		fail(w, 409, "等待配置操作结束超时，未执行删除，请稍后重试")
		return
	}
	defer a.mu.Unlock()
	i := a.serviceIndex(r.PathValue("id"))
	if i < 0 {
		fail(w, 404, "服务不存在")
		return
	}
	if input.Revision != nil && *input.Revision != a.state.Revision {
		fail(w, 409, "配置已变化，请刷新后重新确认删除选项；未调用云端")
		return
	}
	log, err := a.readServiceDeletions()
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	id := a.state.Services[i].ID
	entry := log.Entries[id]
	if entry != nil && (entry.DeleteDNS != input.DeleteDNS || entry.DeleteEdgeOne != input.DeleteEdgeOne || entry.DeleteCloudflare != input.DeleteCloudflare) {
		choice := func(remove bool) string {
			if remove {
				return "删除"
			}
			return "保留"
		}
		fail(w, 409, "此服务已有未结束的删除记录，请按原选项重试（DNS："+choice(entry.DeleteDNS)+"，EdgeOne："+choice(entry.DeleteEdgeOne)+"，Cloudflare："+choice(entry.DeleteCloudflare)+"）；未扩大云端删除范围")
		return
	}
	if entry == nil {
		entry, err = a.planServiceDeletion(ctx, a.state.Services[i], input)
		if err != nil {
			reply(w, 409, map[string]any{"error": "服务未删除，云端未改动：" + err.Error(), "operationStage": "preflight", "cloudChanges": false})
			return
		}
		log.Entries[id] = entry
		if err = a.saveServiceDeletions(log); err != nil {
			fail(w, 500, err.Error())
			return
		}
	}
	if err := a.stopWaitingServicePublication(id); err != nil {
		fail(w, 503, err.Error())
		return
	}
	if err = a.executeServiceDeletion(ctx, log, entry); err != nil {
		fail(w, 409, "服务保留，删除尚未完成；已完成的云端删除不会自动回滚，相关域名已暂停自动发布。"+err.Error())
		return
	}
	old := clone(a.state)
	a.state.Services = append(a.state.Services[:i], a.state.Services[i+1:]...)
	a.state.Revision++
	if err = a.saveLocked(); err != nil {
		a.state = old
		fail(w, 500, "云端选定操作已完成，但服务配置保存失败；服务保留且暂停自动发布，请修复数据目录后保持原选项重试")
		return
	}
	entry.Completed = true
	if err = a.saveServiceDeletions(log); err != nil {
		// An incomplete tombstone is equally restrictive. The service is already
		// gone; do not misrepresent that durable configuration change as a failure.
		entry.Completed = false
	}
	a.replyStateLocked(w)
}

func normalizedDeleteHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
}

func (a *App) planServiceDeletion(ctx context.Context, service model.Service, input serviceDeleteOptions) (*serviceDeletion, error) {
	if service.ServesCloudflare() && input.DeleteEdgeOne {
		return nil, errors.New("Cloudflare 服务不能执行 EdgeOne 删除，请使用 Cloudflare 清理选项")
	}
	entry := &serviceDeletion{ServiceID: service.ID, DeleteDNS: input.DeleteDNS, DeleteEdgeOne: input.DeleteEdgeOne, DeleteCloudflare: input.DeleteCloudflare, Hosts: []string{}, DNS: []serviceDeleteDNS{}, Edge: []serviceDeleteEdge{}}
	journal, err := a.readDNSJournal()
	if err != nil {
		return nil, errors.New("无法读取 DNS 托管记录")
	}
	if journal.Attempt != nil {
		return nil, errors.New("有上次 DNS 写入结果待核对，请先只读预览该域名，再删除服务")
	}
	states := []model.State{a.state}
	if a.state.AppliedAt != "" {
		applied, err := a.appliedSnapshotLocked()
		if err != nil {
			return nil, fmt.Errorf("无法核对已应用服务域名：%w", err)
		}
		states = append(states, applied)
	}
	wanted, shared, bases := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, state := range states {
		bases[normalizedDeleteHost(state.Settings.BaseDomain)] = true
		for _, candidate := range state.Services {
			for _, name := range candidate.Domains {
				host := normalizedDeleteHost(name)
				if candidate.ID == service.ID {
					wanted[host] = wanted[host] || candidate.ServesEdgeOne()
				} else {
					shared[host] = true
				}
			}
		}
	}
	for host := range wanted {
		if pending, err := a.cfDelegationPending(host, ""); err != nil {
			return nil, err
		} else if pending {
			return nil, errors.New("该服务有未完成的 DCV 委派转换，核对完成前不能删除")
		}
		if !shared[host] && !bases[host] {
			entry.Hosts = append(entry.Hosts, host)
		}
	}
	sort.Strings(entry.Hosts)
	if a.opts.DryRun && (input.DeleteDNS || input.DeleteEdgeOne || input.DeleteCloudflare) {
		return nil, errors.New("dry-run 模式不会执行云端删除，请取消云端选项或使用正式运行模式")
	}
	if input.DeleteCloudflare {
		if err := a.planCloudflareServiceDeletion(ctx, states, entry); err != nil {
			return nil, err
		}
	}
	for _, host := range entry.Hosts {
		cnames := []string{}
		if input.DeleteEdgeOne {
			for _, origin := range a.state.EdgeOrigins {
				zone := normalizedDeleteHost(origin.ZoneName)
				if zone == "" || host != zone && !strings.HasSuffix(host, "."+zone) {
					continue
				}
				// A draft-only name is not proof it was ever published at EdgeOne.
				if !wanted[host] && !a.recordedEdgeHost(host, origin.AccountID, origin.ZoneID) {
					continue
				}
				credentials, err := a.edgeAccountCredentials(origin.AccountID)
				if err != nil {
					return nil, fmt.Errorf("%s 的 EdgeOne 账号凭据不可用", host)
				}
				domain, err := a.edgeAPI(credentials).DescribeDomain(ctx, origin.ZoneID, host)
				if err != nil {
					return nil, fmt.Errorf("只读核对 EdgeOne 域名 %s 失败：%w", host, err)
				}
				if domain == nil {
					continue
				}
				target := serviceDeleteEdge{Host: host, AccountID: origin.AccountID, ZoneID: origin.ZoneID, GroupID: origin.GroupID, Cname: domain.Cname}
				if err := validateDeleteEdge(target, domain); err != nil {
					return nil, err
				}
				entry.Edge = append(entry.Edge, target)
				cnames = append(cnames, domain.Cname)
			}
		}
		if managed, ok := journal.Managed[host]; ok {
			remove := input.DeleteDNS
			if input.DeleteCloudflare && managed.Record.Type == dnspod.TypeCNAME {
				for _, cf := range entry.Cloudflare {
					if cf.Host != host {
						continue
					}
					for _, cname := range cf.Cnames {
						remove = remove || sameRecordValue(dnspod.TypeCNAME, managed.Record.Value, cname)
					}
				}
			}
			if input.DeleteEdgeOne && managed.Record.Type == dnspod.TypeCNAME {
				if !input.DeleteDNS && wanted[host] && len(cnames) == 0 && normalizedDeleteHost(managed.Record.Value) != a.baseDomain() {
					return nil, fmt.Errorf("%s 的加速域名不存在或缺少本地归属，无法核对关联 CNAME；如需删除该托管解析，请同时勾选删除 DNS 解析", host)
				}
				for _, cname := range cnames {
					remove = remove || cname != "" && sameRecordValue(dnspod.TypeCNAME, managed.Record.Value, cname)
				}
			}
			target := serviceDeleteDNS{Managed: managed, Delete: remove}
			if remove {
				_, present, err := a.inspectDeleteDNS(ctx, target)
				if err != nil {
					return nil, err
				}
				target.Done = !present
			}
			entry.DNS = append(entry.DNS, target)
		}
	}
	return entry, nil
}

func (a *App) recordedEdgeHost(host, accountID, zoneID string) bool {
	for _, certificate := range a.state.Certificates {
		if certificate.EdgeOne == nil {
			continue
		}
		for _, binding := range certificate.EdgeOne.Bindings {
			if binding.AccountID != accountID || binding.ZoneID != zoneID {
				continue
			}
			for _, name := range binding.Hosts {
				if normalizedDeleteHost(name) == host {
					return true
				}
			}
		}
	}
	return false
}

func validateDeleteEdge(target serviceDeleteEdge, domain *teo.Domain) error {
	if normalizedDeleteHost(domain.Name) != target.Host || domain.ZoneID != target.ZoneID || target.GroupID == "" ||
		domain.Origin == nil || domain.Origin.OriginType != teo.OriginTypeOriginGroup || domain.Origin.Origin != target.GroupID ||
		!sameRecordValue(dnspod.TypeCNAME, target.Cname, domain.Cname) {
		return fmt.Errorf("EdgeOne 域名 %s 的站点、回源或 CNAME 与本机托管记录不一致，拒绝删除；请在控制台核对", target.Host)
	}
	return nil
}

func (a *App) inspectDeleteDNS(ctx context.Context, target serviceDeleteDNS) (dnsprovider.Client, bool, error) {
	managed := target.Managed
	if managed.Host == "" || managed.Record.Key() == "" || managed.AccountID == "" {
		return nil, false, errors.New("DNS 托管记录缺少明确归属，拒绝删除")
	}
	sub, err := labelInZone(managed.Host, managed.Zone)
	if err != nil {
		return nil, false, err
	}
	if managed.Record.Name != sub {
		return nil, false, errors.New("DNS 托管域名与记录名称不一致，拒绝删除")
	}
	credentials, err := a.accountCredentials(managed.AccountID)
	if err != nil {
		return nil, false, fmt.Errorf("%s 的 DNS 账号凭据不可用", managed.Host)
	}
	client := a.dnsAPI(credentials)
	records, err := client.List(ctx, managed.Zone, sub)
	if err != nil {
		return nil, false, fmt.Errorf("只读核对 DNS %s 失败：%w", managed.Host, err)
	}
	for _, record := range records {
		if record.Key() == managed.Record.Key() {
			if !sameDNSRecord(record, managed.Record) {
				return nil, false, fmt.Errorf("DNS %s 的托管记录已被他人修改，拒绝删除", managed.Host)
			}
			return client, true, nil
		}
	}
	return client, false, nil
}

func (a *App) executeServiceDeletion(ctx context.Context, log serviceDeletionLog, entry *serviceDeletion) error {
	if err := a.validateDeletionScope(entry); err != nil {
		return err
	}
	if err := a.executeCloudflareServiceDeletion(ctx, log, entry); err != nil {
		return err
	}
	for i := range entry.Edge {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("本次删除时间预算已用完，请保持原选项继续：%w", err)
		}
		if err := a.executeDeleteEdge(ctx, log, &entry.Edge[i]); err != nil {
			return err
		}
	}
	for i := range entry.DNS {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("本次删除时间预算已用完，请保持原选项继续：%w", err)
		}
		target := &entry.DNS[i]
		if !target.Delete || target.Done {
			continue
		}
		client, present, err := a.inspectDeleteDNS(ctx, *target)
		if err != nil {
			return err
		}
		if present {
			if err := a.authorizeDNSWrite(target.Managed.AccountID); err != nil {
				return err
			}
			if target.Attempt {
				return fmt.Errorf("DNS %s 上次删除结果仍不明确；本次仅只读核对，未重复删除，请在控制台核对", target.Managed.Host)
			}
			target.Attempt = true
			if err := a.saveServiceDeletions(log); err != nil {
				return err
			}
			if err := client.Delete(ctx, target.Managed.Zone, target.Managed.Record.Key()); err != nil {
				if dnsprovider.Rejected(err) {
					target.Attempt = false
					if saveErr := a.saveServiceDeletions(log); saveErr != nil {
						return saveErr
					}
				}
				return fmt.Errorf("DNS %s 删除未确认：%w；可保持原选项重试只读核对", target.Managed.Host, err)
			}
		}
		target.Done, target.Attempt = true, false
		if err := a.saveServiceDeletions(log); err != nil {
			return err
		}
	}
	journal, err := a.readDNSJournal()
	if err != nil || journal.Attempt != nil {
		return errors.New("DNS 托管记录无法安全解除，请先核对本地记录")
	}
	// Release every exclusive host, including unchecked resources. Otherwise a
	// subsequent convergence would interpret the orphan journal as a delete.
	for _, host := range entry.Hosts {
		delete(journal.Managed, host)
	}
	if err := a.saveDNSJournal(journal); err != nil {
		return errors.New("无法保存 DNS 解除托管结果，请保持原选项重试")
	}
	return nil
}

// A retry is a new opportunity for another draft or a settings change to claim
// the name. The recorded authorization must never override that newer owner.
func (a *App) validateDeletionScope(entry *serviceDeletion) error {
	hosts := map[string]bool{}
	for _, host := range entry.Hosts {
		hosts[host] = true
	}
	states := []model.State{a.state}
	if a.state.AppliedAt != "" {
		applied, err := a.appliedSnapshotLocked()
		if err != nil {
			return err
		}
		states = append(states, applied)
	}
	for _, state := range states {
		if hosts[normalizedDeleteHost(state.Settings.BaseDomain)] {
			return errors.New("待删除域名已被设为共享基站，已停止删除；请先解除冲突")
		}
		for _, service := range state.Services {
			if service.ID == entry.ServiceID {
				continue
			}
			for _, host := range service.Domains {
				if hosts[normalizedDeleteHost(host)] {
					return fmt.Errorf("待删除域名 %s 已被其他服务引用，已停止删除；请先解除冲突", host)
				}
			}
		}
	}
	return nil
}

func (a *App) executeDeleteEdge(ctx context.Context, log serviceDeletionLog, target *serviceDeleteEdge) error {
	if target.Done {
		return nil
	}
	credentials, err := a.edgeAccountCredentials(target.AccountID)
	if err != nil {
		return fmt.Errorf("%s 的 EdgeOne 账号凭据不可用", target.Host)
	}
	api := a.edgeAPI(credentials)
	domain, err := api.DescribeDomain(ctx, target.ZoneID, target.Host)
	if err != nil {
		return fmt.Errorf("只读核对 EdgeOne %s 失败：%w", target.Host, err)
	}
	if domain == nil {
		target.Done, target.Attempt = true, ""
		return a.saveServiceDeletions(log)
	}
	if err := validateDeleteEdge(*target, domain); err != nil {
		return err
	}
	if target.Attempt == "delete" || target.Attempt == "offline" && domain.Status != teo.DomainOffline {
		return fmt.Errorf("EdgeOne %s 上次操作结果待核对（当前 %s）；本次未重复写入，请稍后保持原选项重试或在控制台核对", target.Host, domain.Status)
	}
	if domain.Status != teo.DomainOffline {
		if domain.Status != teo.DomainOnline {
			return fmt.Errorf("EdgeOne %s 当前为 %s，尚不能安全删除；请等待云端处理完成后重试", target.Host, domain.Status)
		}
		target.Attempt = "offline"
		if err := a.saveServiceDeletions(log); err != nil {
			return err
		}
		if err := api.SetDomainsOnline(ctx, target.ZoneID, []string{target.Host}, false); err != nil {
			return a.deleteEdgeFailure(log, target, err)
		}
		domain, err = api.DescribeDomain(ctx, target.ZoneID, target.Host)
		if err != nil {
			return fmt.Errorf("EdgeOne %s 停用后只读核对失败：%w", target.Host, err)
		}
		if domain == nil {
			target.Done, target.Attempt = true, ""
			return a.saveServiceDeletions(log)
		}
		if err := validateDeleteEdge(*target, domain); err != nil {
			return err
		}
		if domain.Status != teo.DomainOffline {
			return fmt.Errorf("EdgeOne %s 已提交停用，等待云端变为 offline；请稍后保持原选项重试，本程序不会重复停用", target.Host)
		}
	}
	target.Attempt = "delete"
	if err := a.saveServiceDeletions(log); err != nil {
		return err
	}
	if err := api.DeleteDomain(ctx, target.ZoneID, target.Host); err != nil {
		return a.deleteEdgeFailure(log, target, err)
	}
	// Deletion may be asynchronous. Keep the attempt until a read proves absence.
	domain, err = api.DescribeDomain(ctx, target.ZoneID, target.Host)
	if err != nil || domain != nil {
		return fmt.Errorf("EdgeOne %s 已提交删除，结果待只读核对；请稍后保持原选项重试，不会重复删除", target.Host)
	}
	target.Done, target.Attempt = true, ""
	return a.saveServiceDeletions(log)
}

func (a *App) deleteEdgeFailure(log serviceDeletionLog, target *serviceDeleteEdge, err error) error {
	var refusal *teo.APIError
	knownRefusal := errors.As(err, &refusal) && (refusal.Code == "InvalidParameterValue.InvalidDomainStatus" || refusal.Code == "ResourceInUse")
	if teo.Rejected(err) || knownRefusal {
		target.Attempt = ""
		if saveErr := a.saveServiceDeletions(log); saveErr != nil {
			return saveErr
		}
	}
	return fmt.Errorf("EdgeOne %s 删除未完成：%w；请保持原选项重试，未知结果只会只读核对", target.Host, err)
}
