package app

import (
	"context"
	"net/http"
	"strings"
	"time"

	"w103d/gateway/internal/dnspod"
)

// This endpoint observes only. It neither adopts a record nor grants permission
// to publish. Network failure is unknown, not evidence that a name was removed.
func (a *App) observeDNSRecord(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(r.PathValue("id"))
	result := map[string]any{"host": host, "source": "provider-api", "status": "unknown", "authoritative": "not-checked", "external": "not-checked"}
	defer func() {
		result["checkedAt"] = time.Now().UTC().Format(time.RFC3339)
		reply(w, http.StatusOK, result)
	}()
	a.mu.Lock()
	journal, err := a.readDNSJournal()
	entry, exists := journal.Managed[host]
	if err != nil || !exists {
		a.mu.Unlock()
		result["message"] = "没有可核对的本地托管记录；云端状态未知"
		return
	}
	result["expected"] = entry.Record
	account := a.accountByID(entry.AccountID)
	credentials, err := a.accountCredentials(entry.AccountID)
	pending := dnsUnknownForHost(journal, host)
	a.mu.Unlock()
	if err != nil || account.ID == "" {
		result["message"] = "无法读取核对账号或凭据；云端状态未知"
		return
	}
	sub, err := labelInZone(host, entry.Zone)
	if err != nil || entry.Host != host || entry.Record.Key() == "" {
		result["message"] = "本地托管记录不完整；云端状态未知"
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	records, err := a.dnsAPI(credentials).List(ctx, entry.Zone, sub)
	if err != nil {
		result["message"] = "云端读取失败，未改变本地记录或发布资格"
		return
	}
	a.mu.Lock()
	currentJournal, journalErr := a.readDNSJournal()
	currentEntry, stillManaged := currentJournal.Managed[host]
	currentAccount := a.accountByID(entry.AccountID)
	currentCredentials, credentialErr := a.accountCredentials(entry.AccountID)
	a.mu.Unlock()
	// Scope the snapshot to this name: activity on an unrelated name must not
	// invalidate its observation. Pending mutations are never proof of success,
	// even if the old managed record happens to remain visible at the provider.
	if journalErr != nil || !stillManaged || currentEntry != entry || currentAccount != account || credentialErr != nil || currentCredentials.Fingerprint() != credentials.Fingerprint() {
		result["message"] = "核对期间托管记录、账号或凭据已改变，请重新读取"
		return
	}
	if pending || dnsUnknownForHost(currentJournal, host) {
		result["message"] = "该域名仍有待确认的写入，云端状态未知；请在解析页预览变动并处理"
		return
	}
	result["records"] = records
	result["status"] = "drift"
	relevant := dnsAddressRecords(records)
	if len(relevant) == 1 && sameDNSRecord(relevant[0], entry.Record) && (entry.Record.Type != dnspod.TypeCNAME || len(records) == 1) {
		result["status"] = "matched"
	}
}
