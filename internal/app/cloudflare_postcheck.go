package app

import (
	"net/http"
	"time"
	"w103d/gateway/internal/model"
)

// Explicit post-switch observation; this never modifies DNS, retries cloud writes,
// or grants a preferred target. All network I/O happens outside the state lock.
func (a *App) checkCloudflarePublishedEntry(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Confirm bool `json:"confirm"`
	}
	if !decode(w, r, &input) {
		return
	}
	if !input.Confirm {
		fail(w, 400, "Confirm the bounded IPv4 check of this published hostname")
		return
	}
	if a.opts.DryRun {
		fail(w, 403, "Dry run does not probe external entries")
		return
	}
	host := cfDomain(r.PathValue("host"))
	if !validCFDomain(host) {
		fail(w, 400, "Invalid business hostname")
		return
	}
	applied, err := a.publicAppliedState()
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	var service model.Service
	found := false
	for _, s := range applied.Services {
		if !s.Enabled || !s.ServesCloudflare() {
			continue
		}
		for _, domain := range s.Domains {
			if domain == host {
				service = s
				found = true
				break
			}
		}
	}
	if !found {
		fail(w, 409, "No applied Cloudflare entry for this hostname")
		return
	}
	standard := ""
	for _, account := range applied.CloudflareAccounts {
		if account.ID == service.EdgeAccountID {
			standard = account.StandardTarget
		}
	}
	journal, err := a.readDNSJournal()
	if err != nil {
		fail(w, 409, "Published DNS ledger cannot be read")
		return
	}
	binding, ok := journal.Managed[host]
	if !ok || binding.Record.Type != "CNAME" {
		fail(w, 409, "No locally recorded published CNAME to check")
		return
	}
	before := publishFingerprint(applied)
	checkErr := a.checkCFOriginTargets(r.Context(), host, []string{host})
	latest, err := a.publicAppliedState()
	if err != nil || publishFingerprint(latest) != before {
		fail(w, 409, "Applied configuration changed during the check; discard this observation and check again")
		return
	}
	current, err := a.readDNSJournal()
	currentBinding, exists := current.Managed[host]
	if err != nil || !exists || currentBinding.Record.Value != binding.Record.Value || currentBinding.AccountID != binding.AccountID {
		fail(w, 409, "Published binding changed during the check; check again")
		return
	}
	status, message := "verified", "The currently resolved business entry reached this origin over IPv4 TLS. DNS caches may still contain the previous target; this does not prove global propagation."
	if checkErr != nil {
		status, message = "unverified", checkErr.Error()
	}
	reply(w, 200, map[string]any{"host": host, "status": status, "verified": checkErr == nil, "checkedAt": time.Now().UTC().Format(time.RFC3339), "publishedTarget": binding.Record.Value, "standardTarget": standard, "scope": "local-ipv4-after-switch", "message": message})
}
