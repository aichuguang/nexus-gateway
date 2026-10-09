package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"w103d/gateway/internal/dnspod"
	"w103d/gateway/internal/dnsprovider"
)

type dnsConflict struct {
	RecordID string `json:"recordId"`
	Type     string `json:"type"`
	Value    string `json:"value"`
	TTL      int    `json:"ttl"`
	Name     string `json:"name"`
	Line     string `json:"line"`
	LineID   string `json:"lineId"`
	Status   string `json:"status"`
	Remark   string `json:"remark"`
	Weight   uint64 `json:"weight"`
	Proxied  bool   `json:"proxied"`
}

type dnsRecovery struct {
	Before  []dnsConflict `json:"before"`
	Current []dnsConflict `json:"current"`
	Phase   string        `json:"phase"`
}

func canResumeDNSReplacement(att *dnsAttempt, d dnsDesired) bool {
	return att != nil && (att.Action == dnsReplace || att.Action == dnsCreate || att.Action == dnsUpdate) && att.Replacement != nil &&
		d.Enabled && !d.External && att.Host == strings.ToLower(d.Host) && att.AccountID == d.Account.ID && att.Zone == d.Zone
}

func resumableDNSPlan(plan dnsPlan, records []dnspod.Record) dnsPlan {
	if plan.Action == dnsNoop && plan.Previous != nil && len(dnsAddressRecords(records)) == 1 {
		record := dnsAddressRecords(records)[0]
		plan.Action, plan.Adopt = dnsAdopt, &record
	}
	return plan
}

func (a *App) archiveDNSReplacement(att *dnsAttempt) error {
	id, err := dnsRandomToken()
	if err != nil {
		return err
	}
	return writeJSON(a.dnsPath("dns-replacement-superseded-"+id+".json"), att)
}

// Provider order and equivalent DNS presentation are not changes. Record IDs
// and all material properties remain part of the comparison.
func sameDNSRecords(a, b []dnspod.Record) bool {
	if len(a) != len(b) {
		return false
	}
	used := make([]bool, len(b))
	for _, record := range a {
		found := false
		for i, other := range b {
			if !used[i] && sameDNSRecord(record, other) {
				used[i], found = true, true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func dnsConflictViews(records []dnspod.Record) []dnsConflict {
	out := []dnsConflict{}
	for _, r := range records {
		out = append(out, dnsConflict{r.Key(), r.Type, r.Value, r.TTL, r.Name, r.Line, r.LineID, r.Status, r.Remark, r.Weight, r.Proxied})
	}
	return out
}

// Only the explicit manual preview calls this fallback. Automatic reconciliation
// deliberately continues to use planDNS, which never takes over foreign records.
func planManualDNS(j dnsJournal, d dnsDesired, records []dnspod.Record) (dnsPlan, error) {
	plan, err := planDNS(j, d, records)
	if err == nil {
		return plan, nil
	}
	if !d.Enabled || d.External || len(records) == 0 || len(records) > 100 || dnsUnknownForHost(j, d.Host) {
		return dnsPlan{}, err
	}
	if m, ok := j.Managed[strings.ToLower(d.Host)]; ok && (m.AccountID != d.Account.ID || m.Zone != d.Zone) {
		return dnsPlan{}, err
	}
	if d.Type != dnspod.TypeA && d.Type != dnspod.TypeAAAA && d.Type != dnspod.TypeCNAME {
		return dnsPlan{}, err
	}
	seen := map[string]bool{}
	for _, r := range records {
		if r.Type != dnspod.TypeA && r.Type != dnspod.TypeAAAA && r.Type != dnspod.TypeCNAME {
			return dnsPlan{}, fmt.Errorf("该名称包含 %s 记录，不能安全自动覆盖；请在 DNS 托管商控制台核对", r.Type)
		}
		if r.Key() == "" || seen[r.Key()] || !strings.EqualFold(r.Name, d.Sub) || r.TTL <= 0 || r.Value == "" || r.Status == "" || r.Line == "" || r.LineID == "" {
			return dnsPlan{}, fmt.Errorf("%w；现有 DNS 记录信息不完整，请重新预览", err)
		}
		seen[r.Key()] = true
	}
	return dnsPlan{Action: dnsReplace, Conflicts: append([]dnspod.Record(nil), records...)}, nil
}

// Before is never truncated as deletion progresses: it is the durable recovery
// material even if the process dies after the final delete and before creation.
type dnsReplacement struct {
	Before      []dnspod.Record `json:"before"`
	Phase       string          `json:"phase"`
	DeleteIndex int             `json:"deleteIndex"`
}

func (a *App) replacementFailure(detail string) error {
	return fmt.Errorf("本域名 DNS 覆盖未完成：%s。不会自动重试，不影响其他域名更新；请在解析页重新预览云端现状，再一键强制覆盖，无需先恢复旧记录。完整旧记录及阶段保存在 %s 的 replacement.before 中", detail, a.dnsPath("dns-managed.json"))
}

// Recovery acknowledgement is explicitly user-triggered and never writes DNS.
// It requires all original contents to be restored (new provider IDs are normal)
// and archives the full attempt before clearing protection.
func (a *App) recoverDNSReplacement(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Confirm bool `json:"confirmRecovery"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !in.Confirm {
		fail(w, 400, "必须确认已在托管商恢复原记录，并已核对没有仍在执行的云端操作")
		return
	}
	a.dnsMu.Lock()
	defer a.dnsMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	host := strings.ToLower(r.PathValue("id"))
	j, err := a.readDNSJournal()
	if err != nil {
		fail(w, 500, "无法读取 DNS 恢复记录")
		return
	}
	att := dnsAttemptForHost(j, host)
	if att == nil || att.Replacement == nil {
		fail(w, 409, "没有待恢复的覆盖操作")
		return
	}
	selectDNSAttempt(&j, att.Host, att.AccountID, att.Zone)
	creds, err := a.accountCredentials(att.AccountID)
	if err != nil {
		fail(w, 409, "原 DNS 账号凭据不可用，无法核对恢复结果")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 18*time.Second)
	defer cancel()
	records, err := a.dnsAPI(creds).List(ctx, att.Zone, att.Desired.Name)
	if err != nil {
		fail(w, 502, "读取原 DNS 账号失败，保护仍保留")
		return
	}
	if !sameRestoredDNSRecords(records, att.Replacement.Before) {
		fail(w, 409, "云端尚未完整恢复原记录（类型、值、TTL、线路及备注等必须一致），保护仍保留")
		return
	}
	id, err := dnsRandomToken()
	if err != nil {
		fail(w, 500, "无法创建恢复归档")
		return
	}
	if err := writeJSON(a.dnsPath("dns-replacement-recovered-"+id+".json"), att); err != nil {
		fail(w, 500, "无法保存恢复归档，保护仍保留")
		return
	}
	// Only restore the identity of a record the ledger already owned and which
	// was unchanged in the original snapshot. Foreign records remain foreign.
	if managed, ok := j.Managed[host]; ok && managed.AccountID == att.AccountID && managed.Zone == att.Zone {
		for _, before := range att.Replacement.Before {
			if !sameDNSRecord(managed.Record, before) {
				continue
			}
			for _, current := range records {
				before.ID, before.OpaqueID = current.ID, current.OpaqueID
				if sameDNSRecord(before, current) {
					managed.Record = current
					j.Managed[host] = managed
					break
				}
			}
			break
		}
	}
	j.Attempt = nil
	if err := a.saveDNSJournal(j); err != nil {
		fail(w, 500, "无法保存恢复结果，保护仍保留")
		return
	}
	for token, pending := range a.dnsPending {
		if pending.Host == host {
			delete(a.dnsPending, token)
		}
	}
	reply(w, 200, map[string]any{"ok": true, "message": "已核对原记录恢复并解除覆盖保护；本次未修改云端 DNS。请重新预览。"})
}

func sameRestoredDNSRecords(current, before []dnspod.Record) bool {
	if len(current) != len(before) {
		return false
	}
	used := make([]bool, len(before))
	for _, r := range current {
		if r.Key() == "" {
			return false
		}
		found := false
		for i, old := range before {
			old.ID, old.OpaqueID = r.ID, r.OpaqueID
			if !used[i] && sameDNSRecord(r, old) {
				used[i] = true
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (a *App) publishDNSReplacement(ctx context.Context, client dnsprovider.Client, j *dnsJournal, d dnsDesired, plan dnsPlan) error {
	if err := a.dnsMigrationBlocksHost(d.Host); err != nil {
		return err
	}
	if err := a.authorizeDNSWrite(d.Account.ID); err != nil {
		return err
	}
	if j.Attempt != nil || len(plan.Conflicts) == 0 {
		return refuse("DNS 操作状态发生变化，请重新预览")
	}
	marker, err := dnsRandomToken()
	if err != nil {
		return refuse("无法创建写入标识，未调用云端")
	}
	desired := dnspod.Record{Name: d.Sub, Type: d.Type, Value: d.Value, TTL: d.TTL, Line: dnspod.DefaultLine, LineID: "0", Status: "ENABLE", Remark: "W103D Gateway:" + marker}
	if d.Account.Provider == "alidns" {
		desired.Remark = ""
	}
	j.Attempt = &dnsAttempt{Host: strings.ToLower(d.Host), Zone: d.Zone, AccountID: d.Account.ID, Base: d.Base, Action: dnsReplace, Revision: d.Revision, StartedAt: time.Now().UTC().Format(time.RFC3339), Desired: desired, Replacement: &dnsReplacement{Before: append([]dnspod.Record(nil), plan.Conflicts...), Phase: "delete"}}
	if err := reserveCloudLedger(*j); err != nil {
		return refuse("无法预留 DNS 恢复记录空间，未调用云端")
	}
	if err := a.saveDNSJournal(*j); err != nil {
		return refuse("无法保存 DNS 恢复快照，未调用云端")
	}
	for i, old := range plan.Conflicts {
		j.Attempt.Replacement.DeleteIndex = i
		if err := a.saveDNSJournal(*j); err != nil {
			return a.replacementFailure("无法保存删除阶段")
		}
		// Recheck the remaining snapshot before every destructive step, not only
		// before the first delete. There is no provider-side transaction or CAS.
		current, err := client.List(ctx, d.Zone, d.Sub)
		if err != nil || !sameDNSRecords(current, plan.Conflicts[i:]) {
			return a.replacementFailure("云端记录变化或读取失败")
		}
		if err := client.Delete(ctx, d.Zone, old.Key()); err != nil {
			return a.replacementFailure("删除记录失败或结果未知：" + err.Error())
		}
	}
	current, err := client.List(ctx, d.Zone, d.Sub)
	if err != nil || len(current) != 0 {
		return a.replacementFailure("创建前发现云端记录变化或读取失败")
	}
	j.Attempt.Replacement.Phase = "create"
	if err := a.saveDNSJournal(*j); err != nil {
		return a.replacementFailure("无法保存创建阶段")
	}
	id, err := client.Put(ctx, d.Zone, desired)
	if err != nil || id == "" || len(id) > 1024 {
		return a.replacementFailure("新记录创建失败或结果未知")
	}
	desired.SetKey(id, d.Account.Provider)
	verified, err := client.List(ctx, d.Zone, d.Sub)
	if err != nil || len(verified) != 1 || !sameDNSRecord(verified[0], desired) {
		return a.replacementFailure("新记录已提交，但提交后云端完整记录未匹配，结果待核对")
	}
	j.Managed[strings.ToLower(d.Host)] = dnsManaged{Host: strings.ToLower(d.Host), Zone: d.Zone, AccountID: d.Account.ID, Base: d.Base, Action: dnsReplace, Record: desired}
	j.Attempt = nil
	if err := a.saveDNSJournal(*j); err != nil {
		return a.replacementFailure("新记录已提交，但本地结果保存失败")
	}
	return nil
}

func (a *App) reconcileDNSReplacement(j *dnsJournal, records []dnspod.Record) error {
	att := j.Attempt
	relevant := dnsAddressRecords(records)
	if att.Replacement != nil && att.Replacement.Phase == "create" && len(relevant) == 1 && relevant[0].Key() != "" && (att.Desired.Type != dnspod.TypeCNAME || len(records) == 1) {
		got, expected := relevant[0], att.Desired
		expected.ID, expected.OpaqueID = got.ID, got.OpaqueID
		if sameDNSRecord(got, expected) {
			j.Managed[att.Host] = dnsManaged{Host: att.Host, Zone: att.Zone, AccountID: att.AccountID, Base: att.Base, Action: att.Action, Record: got}
			j.Attempt = nil
			if err := a.saveDNSJournal(*j); err != nil {
				return a.replacementFailure("云端结果已核对但保存失败")
			}
			return nil
		}
	}
	return a.replacementFailure("上次覆盖结果待核对，再次预览不会发出任何写入")
}
