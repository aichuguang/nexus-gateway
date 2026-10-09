package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"slices"
	"time"

	"w103d/gateway/internal/dnspod"
	"w103d/gateway/internal/dnsprovider"
)

type cfDelegationOperation struct {
	Host          string               `json:"host"`
	AccountID     string               `json:"accountId"`
	HostnameID    string               `json:"hostnameId"`
	Name          string               `json:"name"`
	Target        string               `json:"target"`
	DNSAccountID  string               `json:"dnsAccountId"`
	Zone          string               `json:"zone"`
	Before        []dnspod.Record      `json:"before"`
	DeleteIndex   int                  `json:"deleteIndex"`
	DeletePending bool                 `json:"deletePending,omitempty"`
	CreatePending bool                 `json:"createPending,omitempty"`
	Created       *dnspod.Record       `json:"created,omitempty"`
	Desired       *dnspod.Record       `json:"desired,omitempty"`
	PriorRefresh  *cfValidationRefresh `json:"priorRefresh,omitempty"`
	Completed     bool                 `json:"completed"`
}

type cfDelegationOperations struct {
	Format     int                               `json:"format"`
	Operations map[string]*cfDelegationOperation `json:"operations"`
}

func (a *App) readCFDelegations() (cfDelegationOperations, error) {
	log := cfDelegationOperations{Format: 1, Operations: map[string]*cfDelegationOperation{}}
	err := readDNSJSON(a.dnsPath("cloudflare-dcv-delegations.json"), &log)
	if errors.Is(err, os.ErrNotExist) {
		return log, nil
	}
	if err != nil {
		return log, err
	}
	if log.Format != 1 || log.Operations == nil || len(log.Operations) > 200 {
		return log, errors.New("invalid DCV delegation journal")
	}
	for key, op := range log.Operations {
		if op == nil || key != op.AccountID+"/"+op.Host+"/"+op.HostnameID || !validCFDomain(op.Host) || op.Name != "_acme-challenge."+op.Host || op.HostnameID == "" || op.DNSAccountID == "" || op.AccountID == "" || !cfValidDelegationTarget(op.Host, op.Name, op.Target) || op.DeleteIndex < 0 || op.DeleteIndex > len(op.Before) || len(op.Before) > 100 {
			return log, errors.New("invalid DCV delegation scope")
		}
		sub, e := labelInZone(op.Name, op.Zone)
		if e != nil {
			return log, e
		}
		for _, r := range op.Before {
			if r.Type != dnspod.TypeTXT || r.Key() == "" || r.Name != sub {
				return log, errors.New("invalid DCV original record")
			}
		}
		if op.Created != nil && (op.Created.Key() == "" || op.Desired == nil || op.DeleteIndex != len(op.Before) || op.DeletePending || op.Created.Type != dnspod.TypeCNAME || op.Created.Name != sub || !sameRecordValue(dnspod.TypeCNAME, op.Created.Value, op.Target)) {
			return log, errors.New("invalid DCV created record")
		}
		if op.Desired != nil && (op.Desired.Key() != "" || op.Desired.Type != dnspod.TypeCNAME || op.Desired.Name != sub || op.Desired.Value != op.Target || op.Desired.TTL != dnspod.DefaultTTL || op.Desired.Status != "ENABLE" || op.Desired.Line != dnspod.DefaultLine || op.Desired.LineID != "0" || op.Desired.Proxied || op.Desired.Weight != 0) {
			return log, errors.New("invalid DCV desired record")
		}
		if op.Created != nil {
			wanted := *op.Desired
			wanted.ID, wanted.OpaqueID = op.Created.ID, op.Created.OpaqueID
			if !sameDNSRecord(wanted, *op.Created) {
				return log, errors.New("invalid DCV created attributes")
			}
		}
		if op.CreatePending && (op.Desired == nil || op.Created != nil || op.DeleteIndex != len(op.Before) || op.DeletePending) || op.DeletePending && op.DeleteIndex == len(op.Before) || op.Completed && (op.Created == nil || op.DeleteIndex != len(op.Before) || op.DeletePending || op.CreatePending) {
			return log, errors.New("invalid DCV operation phase")
		}
	}
	return log, nil
}

func (a *App) saveCFDelegations(log cfDelegationOperations) error {
	b, err := json.Marshal(log)
	if err != nil {
		return err
	}
	if len(b) > 4<<20 {
		return errors.New("DCV delegation journal capacity exhausted")
	}
	return writeJSON(a.dnsPath("cloudflare-dcv-delegations.json"), log)
}

func (a *App) cfDelegationPending(host, account string) (bool, error) {
	log, err := a.readCFDelegations()
	if err != nil {
		return true, err
	}
	for _, op := range log.Operations {
		if !op.Completed && (host == "" || op.Host == host) && (account == "" || op.DNSAccountID == account || op.AccountID == account) {
			return true, nil
		}
	}
	return false, nil
}

func (a *App) delegateCloudflareDCV(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Confirm  bool  `json:"confirm"`
		Revision int64 `json:"revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !in.Confirm {
		fail(w, 400, "必须确认将本程序验证 TXT 转为 Cloudflare 委派验证 CNAME")
		return
	}
	if a.opts.DryRun {
		fail(w, 403, "dry-run 模式禁止转换验证记录")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(100 * time.Second))
	if !lockContext(ctx, &a.cloudflareMu) {
		fail(w, 409, "Cloudflare 操作繁忙")
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
	s, ok := a.serviceForHost(host)
	if !ok || !s.Enabled || !s.ServesCloudflare() || in.Revision != a.state.Revision {
		fail(w, 409, "服务或配置已变化，请重新确认")
		return
	}
	if blocked, e := a.serviceDeletionBlocked(s.ID); e != nil || blocked {
		fail(w, 409, "服务处于删除保护状态")
		return
	}
	idx := a.cloudflareAccountIndex(s.EdgeAccountID)
	if idx < 0 || !a.state.CloudflareAccounts[idx].Enabled {
		fail(w, 409, "CF 账号不可用")
		return
	}
	account := a.state.CloudflareAccounts[idx]
	credentials, err := a.cloudflareCredentials(account.ID)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	h, err := a.cfAPI(credentials).Host(ctx, account.ZoneID, host)
	if err != nil || h == nil || h.ID == "" || cfDomain(h.Hostname) != host || h.SSL.Method != "txt" {
		fail(w, 409, "无法核对当前 TXT 验证主机名")
		return
	}
	name, target := "_acme-challenge."+host, ""
	for _, d := range h.SSL.DCVDelegationRecords {
		if cfDomain(d.CNAME) == name && cfValidDelegationTarget(host, name, cfDomain(d.CNAMETarget)) {
			if target != "" && target != cfDomain(d.CNAMETarget) {
				fail(w, 409, "CF 返回多个不同委派目标")
				return
			}
			target = cfDomain(d.CNAMETarget)
		}
	}
	if target == "" {
		fail(w, 409, "CF 未提供可用的官方委派验证目标")
		return
	}
	dnsAccount, zone, sub, err := a.resolveZone(ctx, name, false)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	if err := dnspod.ValidateDCVDelegationWrite(zone, sub, target, dnspod.DefaultTTL); err != nil {
		fail(w, 409, err.Error())
		return
	}
	dnsCredentials, err := a.accountCredentials(dnsAccount.ID)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	client := a.dnsAPI(dnsCredentials)
	log, err := a.readCFDelegations()
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	key := account.ID + "/" + host + "/" + h.ID
	op := log.Operations[key]
	for k, v := range log.Operations {
		if k != key && v.Host == host && !v.Completed {
			fail(w, 409, "同域名原委派转换未完成，未扩大操作范围")
			return
		}
	}
	if op == nil {
		records, e := client.List(ctx, zone, sub)
		if e != nil {
			fail(w, 409, e.Error())
			return
		}
		if len(records) == 1 && records[0].Type == dnspod.TypeCNAME && sameRecordValue(dnspod.TypeCNAME, records[0].Value, target) {
			valid, e := a.cfDelegatedValidationLocked(ctx, host, name, h.SSL.DCVDelegationRecords)
			if e != nil || !valid {
				fail(w, 409, "现有委派记录不符合要求")
				return
			}
			reply(w, 200, map[string]any{"ok": true, "host": host, "name": name, "target": target, "reused": true})
			return
		}
		txt, e := a.readDNSTXTJournal()
		if e != nil {
			fail(w, 409, e.Error())
			return
		}
		if txt.Attempt != nil && txt.Attempt.Host == name {
			fail(w, 409, "TXT 写入结果待核对，未转换")
			return
		}
		for _, attempt := range txt.Attempts {
			if attempt.Host == name {
				fail(w, 409, "TXT 写入结果待核对，未转换")
				return
			}
		}
		for _, record := range records {
			owned := false
			for _, m := range txt.Managed {
				owned = owned || m.Action == dnsCreate && m.Zone == zone && m.AccountID == dnsAccount.ID && m.ValidationAccountID == account.ID && m.ValidationHost == host && m.Host == name && sameDNSRecord(m.Record, record)
			}
			if record.Type != dnspod.TypeTXT || !owned {
				fail(w, 409, "验证名称包含非本程序创建的记录，未删除或转换")
				return
			}
		}
		op = &cfDelegationOperation{Host: host, AccountID: account.ID, HostnameID: h.ID, Name: name, Target: target, DNSAccountID: dnsAccount.ID, Zone: zone, Before: records}
		if len(log.Operations) >= 200 {
			fail(w, 409, "委派转换历史已达上限")
			return
		}
		log.Operations[key] = op
		if err := a.saveCFDelegations(log); err != nil {
			fail(w, 500, err.Error())
			return
		}
	}
	if op.Name != name || op.Target != target || op.DNSAccountID != dnsAccount.ID || op.Zone != zone {
		fail(w, 409, "委派配置或解析账号已变化，原转换记录保留")
		return
	}
	if err := a.executeCFDelegation(ctx, client, dnsAccount.Provider, log, op); err != nil {
		fail(w, 409, "委派转换未完成，原记录和阶段已保存；保持相同参数再次核对，不会盲目重试："+err.Error())
		return
	}
	reply(w, 200, map[string]any{"ok": true, "host": host, "name": name, "target": target, "recordId": op.Created.Key()})
}

func (a *App) executeCFDelegation(ctx context.Context, client dnsprovider.Client, provider string, log cfDelegationOperations, op *cfDelegationOperation) error {
	sub, err := labelInZone(op.Name, op.Zone)
	if err != nil {
		return err
	}
	if err := a.authorizeDNSWrite(op.DNSAccountID); err != nil {
		return err
	}
	for op.DeleteIndex < len(op.Before) {
		before := op.Before[op.DeleteIndex]
		records, err := client.List(ctx, op.Zone, sub)
		if err != nil {
			return err
		}
		for _, record := range records {
			if !slices.ContainsFunc(op.Before[op.DeleteIndex:], func(original dnspod.Record) bool { return sameDNSRecord(original, record) }) {
				return errors.New("验证名称出现非快照记录，停止转换")
			}
		}
		present := false
		for _, record := range records {
			if record.Key() == before.Key() {
				if !sameDNSRecord(record, before) {
					return errors.New("原 TXT 已被修改，停止删除")
				}
				present = true
			}
		}
		if present {
			if op.DeletePending {
				return errors.New("上次 TXT 删除尚未确认，本次未重复删除")
			}
			op.DeletePending = true
			if err := a.saveCFDelegations(log); err != nil {
				return err
			}
			if err := client.Delete(ctx, op.Zone, before.Key()); err != nil {
				if dnsprovider.Rejected(err) {
					op.DeletePending = false
					_ = a.saveCFDelegations(log)
				}
				return err
			}
			records, err = client.List(ctx, op.Zone, sub)
			if err != nil {
				return err
			}
			if slices.ContainsFunc(records, func(r dnspod.Record) bool { return r.Key() == before.Key() }) {
				return errors.New("TXT 删除已提交，等待读取确认")
			}
		}
		op.DeletePending = false
		op.DeleteIndex++
		if err := a.saveCFDelegations(log); err != nil {
			return err
		}
	}
	records, err := client.List(ctx, op.Zone, sub)
	if err != nil {
		return err
	}
	if op.Created != nil {
		if len(records) != 1 || !sameDNSRecord(records[0], *op.Created) {
			return errors.New("已创建的委派 CNAME 发生变化")
		}
	} else if len(records) > 0 {
		if !op.CreatePending || op.Desired == nil || len(records) != 1 || records[0].Key() == "" {
			return errors.New("出现未获确认的记录，未继续创建委派 CNAME")
		}
		wanted := *op.Desired
		wanted.ID, wanted.OpaqueID = records[0].ID, records[0].OpaqueID
		if !sameDNSRecord(records[0], wanted) {
			return errors.New("委派 CNAME 与已保存创建意图不一致，未认领记录")
		}
		copy := records[0]
		op.Created = &copy
		op.CreatePending = false
		if err := a.saveCFDelegations(log); err != nil {
			return err
		}
	} else {
		if op.CreatePending {
			return errors.New("上次 CNAME 创建结果未知，未重复创建")
		}
		marker, err := dnsRandomToken()
		if err != nil {
			return err
		}
		desired := dnspod.Record{Name: sub, Type: dnspod.TypeCNAME, Value: op.Target, TTL: dnspod.DefaultTTL, Status: "ENABLE", Line: dnspod.DefaultLine, LineID: "0", Remark: "W103D verification:" + marker}
		if provider == "alidns" {
			desired.Remark = ""
		}
		op.Desired = &desired
		op.CreatePending = true
		if err := a.saveCFDelegations(log); err != nil {
			return err
		}
		id, err := client.Put(ctx, op.Zone, desired)
		if err != nil {
			if dnsprovider.Rejected(err) {
				op.CreatePending = false
				_ = a.saveCFDelegations(log)
			}
			return err
		}
		if id == "" || len(id) > 1024 {
			return errors.New("CNAME 创建未返回有效记录 ID")
		}
		created := desired
		created.SetKey(id, provider)
		op.Created = &created
		op.CreatePending = false
		if err := a.saveCFDelegations(log); err != nil {
			return err
		}
		records, err = client.List(ctx, op.Zone, sub)
		if err != nil {
			return err
		}
		if len(records) != 1 || !sameDNSRecord(records[0], created) {
			return errors.New("CNAME 创建后尚未核对一致")
		}
	}
	if op.Completed {
		return nil
	}
	txt, err := a.readDNSTXTJournal()
	if err != nil {
		return err
	}
	for key, m := range txt.Managed {
		if m.Host == op.Name && m.AccountID == op.DNSAccountID && m.ValidationAccountID == op.AccountID && m.ValidationHost == op.Host {
			if slices.ContainsFunc(op.Before, func(r dnspod.Record) bool { return sameDNSRecord(r, m.Record) }) {
				delete(txt.Managed, key)
			}
		}
	}
	sum := sha256.Sum256([]byte(op.DNSAccountID + "\x00" + op.Name + "\x00" + op.Target))
	txt.Managed[hex.EncodeToString(sum[:])] = dnsManaged{Host: op.Name, Zone: op.Zone, AccountID: op.DNSAccountID, Action: dnsCreate, Record: *op.Created, ValidationAccountID: op.AccountID, ValidationHost: op.Host}
	if err := a.saveDNSTXTJournal(txt); err != nil {
		return err
	}
	cf, err := a.readCloudflareLedger()
	if err != nil {
		return err
	}
	key := op.AccountID + "/" + op.Host
	if previous, ok := cf.ValidationRefreshes[key]; ok && previous.Status == "submitted" {
		op.PriorRefresh = &previous
		if err := a.saveCFDelegations(log); err != nil {
			return err
		}
		delete(cf.ValidationRefreshes, key)
		if err := a.saveCloudflareLedger(cf); err != nil {
			return err
		}
	}
	op.Completed = true
	return a.saveCFDelegations(log)
}
