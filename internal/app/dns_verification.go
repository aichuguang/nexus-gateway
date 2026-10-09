package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"time"

	"w103d/gateway/internal/dnspod"
	"w103d/gateway/internal/dnsprovider"
)

type dnsTXTAttempt struct {
	ValidationAccountID string          `json:"validationAccountId,omitempty"`
	ValidationHost      string          `json:"validationHost,omitempty"`
	Host                string          `json:"host"`
	Zone                string          `json:"zone"`
	AccountID           string          `json:"accountId"`
	Desired             dnspod.Record   `json:"desired"`
	Before              []dnspod.Record `json:"before"`
	StartedAt           string          `json:"startedAt"`
}

type dnsTXTJournal struct {
	Attempts map[string]*dnsTXTAttempt `json:"attempts,omitempty"`
	Managed  map[string]dnsManaged     `json:"managed"`
	Attempt  *dnsTXTAttempt            `json:"attempt,omitempty"`
}

func (a *App) readDNSTXTJournal() (dnsTXTJournal, error) {
	j := dnsTXTJournal{Managed: map[string]dnsManaged{}}
	err := readDNSJSON(a.dnsPath("dns-verification.json"), &j)
	if errors.Is(err, os.ErrNotExist) {
		return j, nil
	}
	if err == nil && j.Managed == nil {
		return j, errors.New("invalid DNS verification journal")
	}
	for key, v := range j.Attempts {
		if v == nil || key != dnsAttemptKey(v.Host, v.AccountID, v.Zone) {
			return j, errors.New("invalid scoped TXT attempt")
		}
	}
	promoteTXTAttempt(&j)
	return j, err
}

func (a *App) saveDNSTXTJournal(j dnsTXTJournal) error {
	return saveCloudLedger(a.dnsPath("dns-verification.json"), j)
}

func (a *App) dnsTXTAccountRemovable(id string) error {
	if pending, err := a.cfDelegationPending("", id); err != nil {
		return err
	} else if pending {
		return errors.New("该账号有未完成的 DCV 委派转换，暂不能删除凭据")
	}
	j, err := a.readDNSTXTJournal()
	if err != nil {
		return errors.New("无法读取 DNS 验证记录保护日志，暂不能删除账号")
	}
	if j.Attempt != nil && j.Attempt.AccountID == id {
		return errors.New("该账号有结果未知的 TXT 验证写入，请先核对")
	}
	for _, attempt := range j.Attempts {
		if attempt.AccountID == id {
			return errors.New("account has an unresolved TXT operation")
		}
	}
	return nil
}

func validateTXTRecords(records []dnspod.Record, value string) (bool, error) {
	exists := false
	for _, r := range records {
		if r.Type == dnspod.TypeCNAME {
			return false, errors.New("验证名称已有 CNAME，不能同时写入 TXT；请核对现有验证委派，不会覆盖")
		}
		if r.Type == dnspod.TypeTXT && r.Value == value {
			if r.Status != "ENABLE" || r.Line != dnspod.DefaultLine || r.LineID != "0" {
				return false, errors.New("相同验证 token 的现有 TXT 未启用或不在默认线路，请手动核对")
			}
			exists = true
		}
	}
	return exists, nil
}

// previewDNSTXTLocked performs only account/zone and record compatibility reads.
// Caller holds mu and dnsMu, as for ensureDNSTXTLocked.
func (a *App) previewDNSTXTLocked(ctx context.Context, host, value string) error {
	account, zone, sub, err := a.resolveZone(ctx, strings.ToLower(strings.TrimSuffix(host, ".")), false)
	if err != nil {
		return err
	}
	if err = dnspod.ValidateVerificationWrite(zone, sub, value, dnspod.DefaultTTL); err != nil {
		return err
	}
	credentials, err := a.accountCredentials(account.ID)
	if err != nil {
		return err
	}
	records, err := a.dnsAPI(credentials).List(ctx, zone, sub)
	if err != nil {
		return err
	}
	_, err = validateTXTRecords(records, value)
	return err
}

// ensureDNSTXTLocked creates only a missing token. Other TXT values are never
// overwritten or removed, even when they share an ACME validation label.
// A durable before-snapshot precedes the one mutation; unknown outcomes are
// read back, never automatically replayed. Caller holds mu and dnsMu.
func (a *App) ensureDNSTXTLocked(ctx context.Context, host, value string, owner ...string) error {
	ownerAccount, ownerHost := "", ""
	if len(owner) == 2 {
		ownerAccount, ownerHost = owner[0], owner[1]
	}
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	account, zone, sub, err := a.resolveZone(ctx, host, false)
	if err != nil {
		return err
	}
	if err = dnspod.ValidateVerificationWrite(zone, sub, value, dnspod.DefaultTTL); err != nil {
		return err
	}
	credentials, err := a.accountCredentials(account.ID)
	if err != nil {
		return err
	}
	client := a.dnsAPI(credentials)
	j, err := a.readDNSTXTJournal()
	if err != nil {
		return errors.New("无法读取 TXT 写入保护日志，未调用云端写入")
	}
	selectTXTAttempt(&j, host, account.ID, zone)
	if j.Attempt != nil && (j.Attempt.Host != host || j.Attempt.AccountID != account.ID || j.Attempt.Zone != zone || j.Attempt.Desired.Value != value) {
		return errors.New("另一个 TXT 验证写入结果未知，请先核对原验证任务")
	}
	records, err := client.List(ctx, zone, sub)
	if err != nil {
		return err
	}
	exists, err := validateTXTRecords(records, value)
	if err != nil {
		return err
	}
	keySum := sha256.Sum256([]byte(account.ID + "\x00" + host + "\x00" + value))
	key := hex.EncodeToString(keySum[:])
	if unknown, err := a.txtCleanupUnknown(key); err != nil {
		return err
	} else if unknown {
		return errors.New("TXT has an unresolved legacy delete; verify the original record with the DNS provider before resuming management")
	}
	if j.Attempt != nil {
		before := map[string]bool{}
		for _, r := range j.Attempt.Before {
			before[r.Key()] = true
		}
		matches := []dnspod.Record{}
		for _, r := range records {
			wanted := j.Attempt.Desired
			wanted.ID, wanted.OpaqueID = r.ID, r.OpaqueID
			if !before[r.Key()] && sameDNSRecord(r, wanted) {
				matches = append(matches, r)
			}
		}
		if len(matches) != 1 {
			return errors.New("上次 TXT 创建结果仍不明确，本次仅只读核对；不会重复创建，请在 DNS 控制台核对")
		}
		j.Managed[key] = dnsManaged{Host: host, Zone: zone, AccountID: account.ID, Action: dnsCreate, Record: matches[0], ValidationAccountID: j.Attempt.ValidationAccountID, ValidationHost: j.Attempt.ValidationHost}
		j.Attempt = nil
		if err = a.saveDNSTXTJournal(j); err != nil {
			return errors.New("TXT 结果已核对，但本地日志保存失败")
		}
		return nil
	}
	if exists {
		return nil
	}
	marker, err := dnsRandomToken()
	if err != nil {
		return err
	}
	desired := dnspod.Record{Name: sub, Type: dnspod.TypeTXT, Value: value, TTL: dnspod.DefaultTTL, Status: "ENABLE", Line: dnspod.DefaultLine, LineID: "0", Remark: "W103D verification:" + marker}
	if account.Provider == "alidns" {
		desired.Remark = ""
	}
	j.Attempt = &dnsTXTAttempt{Host: host, Zone: zone, AccountID: account.ID, Desired: desired, Before: records, StartedAt: time.Now().UTC().Format(time.RFC3339), ValidationAccountID: ownerAccount, ValidationHost: ownerHost}
	if err := a.authorizeDNSWrite(account.ID); err != nil {
		return err
	}
	if err := reserveCloudLedger(j); err != nil {
		return err
	}
	if err = a.saveDNSTXTJournal(j); err != nil {
		return errors.New("无法保存 TXT 写前保护日志，未调用云端")
	}
	id, err := client.Put(ctx, zone, desired)
	if err == nil && (id == "" || len(id) > 1024) {
		err = errors.New("provider returned an invalid record ID")
	}
	if err != nil {
		if dnsprovider.Rejected(err) {
			j.Attempt = nil
			if a.saveDNSTXTJournal(j) == nil {
				return err
			}
		}
		return errors.New("TXT 验证创建结果未知，已阻止自动重试；继续发布只会核对云端，不会重复写入")
	}
	desired.SetKey(id, account.Provider)
	j.Managed[key] = dnsManaged{Host: host, Zone: zone, AccountID: account.ID, Action: dnsCreate, Record: desired, ValidationAccountID: ownerAccount, ValidationHost: ownerHost}
	j.Attempt = nil
	if err = a.saveDNSTXTJournal(j); err != nil {
		return errors.New("TXT 创建成功但本地日志保存失败，请继续只读核对")
	}
	return nil
}
