package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"w103d/gateway/internal/dnspod"
)

const cfHTTPWaitingMessage = "业务 CNAME 已发布，Cloudflare 正在尝试自动 HTTP 验证；证书尚未就绪，HTTPS 可能暂不可用，系统会继续检查"

// This receipt authorizes one create-only DNS publication, not certificate
// readiness or a lasting exception to the ordinary DNS publication gate.
type cfHTTPValidation struct {
	Host         string         `json:"host"`
	ServiceID    string         `json:"serviceId"`
	AccountID    string         `json:"accountId"`
	HostnameID   string         `json:"hostnameId"`
	JobID        string         `json:"jobId"`
	Fingerprint  string         `json:"fingerprint"`
	DNSAccountID string         `json:"dnsAccountId"`
	Zone         string         `json:"zone"`
	Target       string         `json:"target"`
	StartedAt    string         `json:"startedAt"`
	Attempted    bool           `json:"attempted"`
	Submitted    bool           `json:"submitted"`
	Record       *dnspod.Record `json:"record,omitempty"`
}

type cfHTTPValidations struct {
	Format  int                          `json:"format"`
	Entries map[string]*cfHTTPValidation `json:"entries"`
}

func (a *App) saveCFHTTPValidations(j cfHTTPValidations) error {
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if len(b) > (4<<20)-(32<<10) {
		return errors.New("HTTP 验证授权记录容量不足，未继续写入")
	}
	return writeJSON(a.dnsPath("cloudflare-http-validations.json"), j)
}

func (a *App) readCFHTTPValidations() (cfHTTPValidations, error) {
	j := cfHTTPValidations{Format: 1, Entries: map[string]*cfHTTPValidation{}}
	err := readDNSJSON(a.dnsPath("cloudflare-http-validations.json"), &j)
	if errors.Is(err, os.ErrNotExist) {
		return j, nil
	}
	if err != nil {
		return j, err
	}
	if j.Format != 1 || j.Entries == nil || len(j.Entries) > 200 {
		return j, errors.New("HTTP 验证授权记录不合法")
	}
	for key, op := range j.Entries {
		if op == nil || !validCFDomain(op.Host) || !validCFDomain(op.Target) || !validCFDomain(op.Zone) || op.ServiceID == "" || op.AccountID == "" || op.HostnameID == "" || op.DNSAccountID == "" || op.JobID == "" || len(op.Fingerprint) != 64 || key != op.AccountID+"/"+op.Host+"/"+op.HostnameID {
			return j, errors.New("HTTP 验证授权范围不合法")
		}
		sub, err := labelInZone(op.Host, op.Zone)
		if err != nil {
			return j, err
		}
		if _, err := time.Parse(time.RFC3339, op.StartedAt); err != nil {
			return j, err
		}
		if op.Submitted && (!op.Attempted || op.Record == nil) {
			return j, errors.New("HTTP 验证提交记录不完整")
		}
		if r := op.Record; r != nil && (r.Key() == "" || r.Name != sub || r.Type != dnspod.TypeCNAME || !sameRecordValue(r.Type, r.Value, op.Target) || r.TTL != dnspod.DefaultTTL || r.Status != "ENABLE" || r.Line != dnspod.DefaultLine || r.LineID != "0" || r.Proxied || r.Weight != 0) {
			return j, errors.New("HTTP 验证 DNS 记录不合法")
		}
	}
	return j, nil
}

// Called with mu held, including on a read-only cloud snapshot.
func (a *App) cfHTTPValidationSubmitted(host, account string) (bool, error) {
	j, err := a.readCFHTTPValidations()
	if err != nil {
		return false, err
	}
	s, ok := a.serviceForHost(host)
	if !ok || !s.ServesCloudflare() || s.EdgeAccountID != account {
		return false, nil
	}
	for _, op := range j.Entries {
		if op.Host != host || op.AccountID != account || op.ServiceID != s.ID || !op.Submitted || op.Record == nil || op.Fingerprint != publishContinuationFingerprint(a.state, s.ID) {
			continue
		}
		cf, err := a.readCloudflareLedger()
		if err != nil {
			return false, err
		}
		if resource := cf.Resources[cfDeletionKey(account, host)]; resource.ID != op.HostnameID || resource.Pending || !resource.Owned {
			continue
		}
		dns, err := a.readDNSJournal()
		if err != nil {
			return false, err
		}
		m, exists := dns.Managed[host]
		if exists && m.AccountID == op.DNSAccountID && m.Zone == op.Zone && sameDNSRecord(m.Record, *op.Record) {
			return true, nil
		}
	}
	return false, nil
}

func (a *App) cfHTTPWaitingJob(serviceID string) (publishJob, error) {
	a.jobs.mu.Lock()
	defer a.jobs.mu.Unlock()
	a.loadPublishJobsLocked()
	if a.jobs.err != nil {
		return publishJob{}, a.jobs.err
	}
	for _, job := range a.jobs.log.Jobs {
		if job.ServiceID != serviceID || !publishJobActive(job.Status) {
			continue
		}
		if job.Status != "waiting" || job.ContinuationFingerprint != publishContinuationFingerprint(a.state, serviceID) {
			return publishJob{}, errors.New("发布正在执行或配置已变化，不能插入 HTTP 验证")
		}
		started, err := time.Parse(time.RFC3339, job.WaitStartedAt)
		if err != nil || time.Since(started) >= publishWaitLimit {
			return publishJob{}, errors.New("原发布等待已超时，请按当前配置重新发布后再确认 HTTP 验证")
		}
		return job, nil
	}
	return publishJob{}, errors.New("请先通过服务自动发布进入证书等待状态，再确认 HTTP 验证")
}

func (a *App) startCloudflareHTTPValidation(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Confirm  bool  `json:"confirm"`
		Revision int64 `json:"revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !in.Confirm {
		fail(w, 400, "必须确认先发布业务 CNAME，证书生效前 HTTPS 可能暂不可用")
		return
	}
	if a.opts.DryRun {
		fail(w, 403, "dry-run 禁止发布 HTTP 验证入口")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 75*time.Second)
	defer cancel()
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(85 * time.Second))
	if !lockContext(ctx, &a.cloudflareMu) {
		fail(w, 409, "CF 操作繁忙")
		return
	}
	defer a.cloudflareMu.Unlock()
	if !lockContext(ctx, &a.dnsMu) {
		fail(w, 409, "DNS 操作繁忙")
		return
	}
	defer a.dnsMu.Unlock()
	if !lockContext(ctx, &a.mu) {
		fail(w, 409, "配置操作繁忙")
		return
	}
	defer a.mu.Unlock()
	host := cfDomain(r.PathValue("host"))
	service, ok := a.serviceForHost(host)
	if !validCFDomain(host) || !ok || !service.Enabled || !service.ServesCloudflare() || service.UsePreferred || in.Revision != a.state.Revision {
		fail(w, 409, "服务或配置已变化；HTTP 验证仅支持已启用的 CF 标准入口")
		return
	}
	job, err := a.cfHTTPWaitingJob(service.ID)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	// The complete normal preflight is read-only: it proves applied origin TLS,
	// strict mode, shared resources, deletion/delegation guards and account scope.
	plan, err := a.cfReconcileLocked(ctx, host, false, true)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	if plan.HostnameStatus != "active" || plan.OriginStatus != "active" || plan.Target != plan.StandardTarget || !validCFDomain(plan.Target) {
		fail(w, 409, "CF 主机名、严格回源和标准入口尚未就绪，未发布 CNAME")
		return
	}
	for _, step := range plan.Steps {
		if step.Action != "ensure-validation-txt" && step.Action != "use-dcv-delegation" {
			fail(w, 409, "接入资源需要其他变更，本操作不会替换共享资源或 SSL 设置")
			return
		}
	}
	idx := a.cloudflareAccountIndex(service.EdgeAccountID)
	if idx < 0 {
		fail(w, 409, "CF 账号已移除")
		return
	}
	account := a.state.CloudflareAccounts[idx]
	c, err := a.cloudflareCredentials(account.ID)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	api := a.cfAPI(c)
	mode, err := cfReadSSLMode(ctx, api, account.ZoneID)
	if err != nil || mode != "strict" {
		fail(w, 409, "无法确认 Full (strict)，未发布 CNAME")
		return
	}
	h, err := api.Host(ctx, account.ZoneID, host)
	if err != nil || h == nil || h.ID == "" || cfDomain(h.Hostname) != host || h.Status != "active" || h.CustomOriginServer != "" || h.CustomOriginSNI != "" || h.SSL.Wildcard != nil && *h.SSL.Wildcard {
		fail(w, 409, "无法确认当前非通配符 CF 主机名，未发布 CNAME")
		return
	}
	ledger, err := a.readCloudflareLedger()
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	for key, resource := range ledger.Resources {
		if strings.HasPrefix(key, account.ID+"/") && resource.Pending {
			fail(w, 409, "CF 写入结果待核对，未开始 HTTP 验证")
			return
		}
	}
	owned := ledger.Resources[cfDeletionKey(account.ID, host)]
	if !owned.Owned || owned.Kind != "hostname" || owned.ID != h.ID || owned.Value != host || owned.ZoneID != "" && owned.ZoneID != account.ZoneID {
		fail(w, 409, "主机名缺少匹配的创建归属，未发布 CNAME")
		return
	}
	if err := a.dnsMigrationBlocksHost(host); err != nil {
		fail(w, 409, err.Error())
		return
	}
	dnsAccount, zone, sub, err := a.resolveZone(ctx, host, false)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	if err := a.authorizeDNSWrite(dnsAccount.ID); err != nil {
		fail(w, 409, err.Error())
		return
	}
	if err := dnspod.ValidateWrite(zone, sub, dnspod.TypeCNAME, plan.StandardTarget, dnspod.DefaultTTL); err != nil {
		fail(w, 409, err.Error())
		return
	}
	credentials, err := a.accountCredentials(dnsAccount.ID)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	client := a.dnsAPI(credentials)
	records, err := client.List(ctx, zone, sub)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	j, err := a.readDNSJournal()
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	ops, err := a.readCFHTTPValidations()
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	key := account.ID + "/" + host + "/" + h.ID
	op := ops.Entries[key]
	d := dnsDesired{Host: host, ServiceID: service.ID, Enabled: true, Account: dnsAccount, Zone: zone, Sub: sub, Type: dnspod.TypeCNAME, Value: plan.StandardTarget, TTL: dnspod.DefaultTTL, Revision: a.state.AppliedRevision}
	if op != nil && (op.ServiceID != service.ID || op.Fingerprint != job.ContinuationFingerprint || op.DNSAccountID != dnsAccount.ID || op.Zone != zone || op.Target != d.Value) {
		fail(w, 409, "原 HTTP 验证范围已变化，未扩大或重复操作")
		return
	}
	wrote := false
	if op == nil || !op.Attempted {
		if h.SSL.Status == "active" || !strings.HasPrefix(h.SSL.Status, "pending_") {
			fail(w, 409, "当前证书不在待验证/签发状态，请使用普通发布或先核对证书错误")
			return
		}
		if _, exists := j.Managed[host]; exists || len(records) > 0 || dnsUnknownForHost(j, host) {
			fail(w, 409, "该业务域名已有解析、托管或未知写入；HTTP 验证只创建空名称，不覆盖或认领现有记录")
			return
		}
		if op == nil {
			if len(ops.Entries) >= 200 {
				fail(w, 409, "HTTP 验证记录已达上限")
				return
			}
			op = &cfHTTPValidation{Host: host, ServiceID: service.ID, AccountID: account.ID, HostnameID: h.ID, JobID: job.ID, Fingerprint: job.ContinuationFingerprint, DNSAccountID: dnsAccount.ID, Zone: zone, Target: d.Value, StartedAt: time.Now().UTC().Format(time.RFC3339)}
			ops.Entries[key] = op
		}
		// Do not leave a stale "business DNS not switched" step after a request
		// that might create it but lose its response. Success is reported below.
		if err := a.updatePublishJob(job.ID, func(j *publishJob) {
			for i := range j.Steps {
				step := &j.Steps[i]
				if step.Kind == "dns" && step.Host == host {
					step.OK = false
					step.Status = "waiting"
					step.Message = "已确认尝试自动 HTTP 验证，CNAME 提交结果以云端核对为准"
					step.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
				}
			}
		}); err != nil {
			fail(w, 500, "无法保存 HTTP 验证进度，未调用 DNS 写入")
			return
		}
		op.Attempted = true
		if err := a.saveCFHTTPValidations(ops); err != nil {
			fail(w, 500, err.Error())
			return
		}
		// No readiness flag is forged. Only this explicitly consented, empty-name
		// path builds a create-only plan before the edge certificate is active.
		err = a.commitDNS(ctx, d, dnsPreview{Host: host, AccountID: dnsAccount.ID, Zone: zone, SubDomain: sub, Type: d.Type, Value: d.Value, TTL: d.TTL, Action: dnsCreate, Revision: d.Revision, snapshot: records})
		if err != nil {
			var refusal dnsRefusal
			if errors.As(err, &refusal) {
				// Only a proven pre-write refusal can release this intent. A lost
				// provider response must remain attempted and readback-only.
				op.Attempted = false
				if saveErr := a.saveCFHTTPValidations(ops); saveErr != nil {
					fail(w, 500, "DNS 未写入，但无法保存可重试状态："+saveErr.Error())
					return
				}
				fail(w, 409, "HTTP 验证入口检查未通过，未发送 DNS 写入；核对后可再次确认："+err.Error())
				return
			}
			fail(w, 409, "HTTP 验证入口提交未完成，已保留写前记录；再次确认仅核对结果，不会重复创建："+err.Error())
			return
		}
		wrote = true
		j, err = a.readDNSJournal()
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		records, err = client.List(ctx, zone, sub)
		if err != nil {
			fail(w, 409, "CNAME 已提交，但读回暂不可用；未判定验证成功")
			return
		}
	}
	if attempt := dnsAttemptForHost(j, host); attempt != nil {
		if attempt.Action != dnsCreate || attempt.AccountID != dnsAccount.ID || attempt.Zone != zone || attempt.Desired.Type != d.Type || attempt.Desired.Name != sub || attempt.Desired.Value != d.Value || attempt.Desired.TTL != d.TTL || attempt.Replacement != nil || len(records) != 1 {
			fail(w, 409, "原 CNAME 写入结果仍待核对，未重复创建")
			return
		}
		if err := a.reconcileDNS(&j, records, d); err != nil {
			fail(w, 409, err.Error())
			return
		}
	}
	m, exists := j.Managed[host]
	if !exists || m.AccountID != dnsAccount.ID || m.Zone != zone || m.Action != dnsCreate || len(records) != 1 || !sameDNSRecord(records[0], m.Record) || !recordMatchesDesired(m.Record, d) || op.Record != nil && !sameDNSRecord(*op.Record, m.Record) {
		fail(w, 409, "原 HTTP CNAME 尚未精确核对或已被修改；不会重复创建/覆盖")
		return
	}
	record := m.Record
	op.Record = &record
	op.Submitted = true
	if err := a.saveCFHTTPValidations(ops); err != nil {
		fail(w, 500, err.Error())
		return
	}
	err = a.updatePublishJob(job.ID, func(job *publishJob) {
		job.Message = cfHTTPWaitingMessage
		job.NextAttemptAt = time.Now().Add(10 * time.Second).UTC().Format(time.RFC3339)
		found := false
		for i := range job.Steps {
			step := &job.Steps[i]
			if step.Host != host {
				continue
			}
			if step.Kind == "cloudflare" {
				step.Message = cfHTTPWaitingMessage
			}
			if step.Kind == "dns" {
				step.OK = true
				step.Changes = wrote
				step.Action = dnsCreate
				step.Status = "succeeded"
				step.Message = "已按授权创建业务 CNAME，用于自动 HTTP 验证；证书和 HTTPS 仍待就绪"
				step.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
				found = true
			}
		}
		if !found {
			job.Steps = append(job.Steps, publishJobStep{publishStep: publishStep{Kind: "dns", Host: host, Role: "HTTP 验证入口", Action: dnsCreate, OK: true, Changes: wrote, Message: cfHTTPWaitingMessage}, Status: "succeeded", UpdatedAt: time.Now().UTC().Format(time.RFC3339)})
		}
	})
	if err != nil {
		fail(w, 500, "CNAME 已发布，但发布进度保存失败，请只读核对")
		return
	}
	reply(w, 200, map[string]any{"ok": true, "host": host, "target": d.Value, "jobId": job.ID, "dnsPublished": true, "certificateReady": plan.Ready, "message": cfHTTPWaitingMessage})
}
