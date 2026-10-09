package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Automatic convergence. The public address of a home line changes on the ISP's
// schedule, not the operator's; a gateway that only publishes when someone
// clicks goes dark for as long as it takes that person to notice.
//
// The ordering is the whole design. The new address must be listening and
// locally verified *before* any name is republished, because the reverse order
// publishes a name that points at nothing — a black hole that resolves cleanly
// and fails silently, which is worse than an obvious NXDOMAIN. Every step below
// is therefore: prove the new entry works, and only then tell the world.

const (
	// How often the (cheap, local) address check runs.
	convergenceProbeInterval = time.Minute
	// How often names are re-read from the provider to repair drift. Listing
	// costs a cloud call per name, so it is deliberately much rarer.
	convergenceReconcileInterval = 30 * time.Minute
	// After a failed convergence, retry sooner than the full reconcile interval.
	convergenceRetryInterval = 5 * time.Minute
)

// convergenceStatus is what the interface shows about the automatic path. It is
// deliberately explicit about failure: silence would be indistinguishable from
// "nothing needed doing".
type convergenceStatus struct {
	Enabled   bool   `json:"enabled"`
	CheckedAt string `json:"checkedAt,omitempty"`
	Changed   bool   `json:"changed"`
	Address   string `json:"address,omitempty"`
	Published int    `json:"published"`
	Error     string `json:"error,omitempty"`
}

// convergenceState reads what the last pass reported. Callers hold no locks.
func (a *App) convergenceState() convergenceStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.convergence
}

// noteConvergenceError adds a failure from the EdgeOne half without erasing what
// the DNS half already reported about the same pass. The two halves fail
// independently, and a status that mentioned only one of them would send an
// operator to look in the wrong system.
func (a *App) noteConvergenceError(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.convergence.Error == "" {
		a.convergence.Error = err.Error()
		return
	}
	a.convergence.Error += "；" + err.Error()
}

func (a *App) dnsStatus(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	status := a.convergence
	status.Enabled = a.state.Settings.AutoResolve
	status.Address = a.state.AppliedPublicAddress
	reply(w, 200, status)
}

// A failed cloud phase remains due even after the local address has moved.
// Keep the schedule separate from the amount of work: a successful read-only
// reconciliation also earns the full interval before the next cloud read.
type convergenceSchedule struct {
	reconcileDue time.Time
	retry        bool
}

func (s convergenceSchedule) due(now time.Time) bool {
	return s.retry || !now.Before(s.reconcileDue)
}

func (s *convergenceSchedule) complete(now time.Time, reconciled, changed bool, err error) time.Duration {
	s.retry = err != nil
	if s.retry {
		return convergenceRetryInterval
	}
	if reconciled || changed {
		s.reconcileDue = now.Add(convergenceReconcileInterval)
	}
	return convergenceProbeInterval
}

// The phases keep their own locks. Retrying repeats the read/reconcile path,
// never an unverified cloud write, and never forgets the EdgeOne half.
func (a *App) convergencePass(ctx context.Context, reconcile bool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !a.changeMu.TryLock() {
		return false, errGatewayChanging
	}
	defer a.changeMu.Unlock()
	reconcile = reconcile || a.convergenceState().Error != ""
	changed, err := a.converge(ctx, reconcile)
	if err != nil {
		return changed, err
	}
	if a.convergenceState().Changed || reconcile {
		updated, err := a.convergeEdge(ctx)
		if err != nil {
			a.noteConvergenceError(err)
			return changed || updated > 0, err
		}
		changed = changed || updated > 0
	}
	return changed, nil
}

// startConvergence runs the automatic public-address path in the background.
// Its stop function only cancels; the shared lifecycle drain waits for completion.
func (a *App) startConvergence() func() {
	parent, finish, accepted := a.beginWork()
	if !accepted {
		return func() {}
	}
	parent, stop := context.WithCancel(parent)
	go func() {
		defer finish()
		defer stop()
		// A short first delay lets the management server finish starting before
		// anything is allowed to touch the running proxy configuration.
		next := time.NewTimer(20 * time.Second)
		defer next.Stop()
		// On restart the proxy may already have moved while cloud publication
		// was interrupted, so the first pass reconciles instead of waiting 30m.
		schedule := convergenceSchedule{reconcileDue: time.Now()}
		for {
			select {
			case <-parent.Done():
				return
			case <-next.C:
			}
			ctx, cancel := context.WithTimeout(parent, 90*time.Second)
			reconcile := schedule.due(time.Now())
			changed, err := a.convergencePass(ctx, reconcile)
			cancel()
			if parent.Err() != nil {
				return
			}
			if err == errGatewayChanging {
				next.Reset(convergenceProbeInterval)
				continue
			}
			next.Reset(schedule.complete(time.Now(), reconcile, changed, err))
		}
	}()
	return stop
}

// converge performs one automatic pass. It reports whether it changed anything,
// which is what decides how soon the next expensive pass is due.
func (a *App) converge(ctx context.Context, reconcileRecords bool) (bool, error) {
	if !lockContext(ctx, &a.dnsMu) {
		return false, ctx.Err()
	}
	defer a.dnsMu.Unlock()
	if !lockContext(ctx, &a.mu) {
		return false, ctx.Err()
	}
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !a.state.Settings.AutoResolve {
		return false, nil
	}
	reconcileRecords = reconcileRecords || a.convergence.Error != ""
	// Every exit records a status. A pass that fails silently is worse than one
	// that fails loudly: the operator cannot tell it apart from "nothing needed
	// doing", which is exactly the state that looks like the feature working.
	fail := func(err error) (bool, error) {
		a.convergence = convergenceStatus{CheckedAt: time.Now().UTC().Format(time.RFC3339), Error: err.Error()}
		return false, err
	}
	address, moved, err := a.convergeAddressLocked(ctx)
	if err != nil {
		return fail(err)
	}
	a.convergence = convergenceStatus{CheckedAt: time.Now().UTC().Format(time.RFC3339), Changed: moved, Address: address}
	if !moved && !reconcileRecords {
		return false, nil
	}
	published, err := a.convergeRecordsLocked(ctx, moved)
	a.convergence.Published = published
	if err != nil {
		a.convergence.Error = err.Error()
		return moved || published > 0, err
	}
	return moved || published > 0, nil
}

// convergeAddressLocked moves the published entry onto whatever address the line
// currently holds, and applies the change before returning.
//
// It deliberately does not wait for an operator click. The address it moves to
// is this device's own current address, every service moves together (exactly
// what the ISP's prefix rotation would have done anyway), and the change is
// rolled back unless the new entry applies and passes its TLS check. Refusing
// here would mean an operator whose prefix rotated has to disable every service
// before the gateway will repair itself.
//
// It refuses to apply a pending draft, though. Re-rendering for a new address is
// one thing; silently deploying half-finished editing work is another, and the
// operator would have no way to know it happened.
func (a *App) convergeAddressLocked(ctx context.Context) (string, bool, error) {
	selected, _, err := a.selectPublicAddress(ctx)
	if err != nil {
		return "", false, err
	}
	if selected == "" {
		if a.state.AppliedPublicAddress == "" {
			return "", false, nil
		}
		return "", false, fmt.Errorf("本机已没有可用的公网地址；代理仍停留在 %s，未做改动", a.state.AppliedPublicAddress)
	}
	if selected == a.state.AppliedPublicAddress {
		return selected, false, nil
	}
	// A draft is only a reason to stop when applying it would deploy something the
	// operator has not seen. A revision that moved without changing what the proxy
	// serves — an entry mode flip, say — must not hold up an address change:
	// refusing there leaves the proxy listening on an address the line no longer
	// has while the operator is told to apply a draft that changes nothing.
	before := a.state.AppliedPublicAddress
	if err := a.maintainAppliedAddressLocked(ctx, selected); err != nil {
		return "", false, fmt.Errorf("公网地址 %s 未能应用（此前记录为 %s）：%w", selected, before, err)
	}
	return a.state.AppliedPublicAddress, true, nil
}

// convergeRecordsLocked maintains previously authorized address records against
// the applied proxy, preserving published CNAME targets and unpublished drafts.
// Withdrawal is explicit. Failures stop only the affected name, not other names
// sharing the same account or the base station they depend on.
//
// moved says the address itself changed, which orders the work: the base station
// goes first, because every directly published name points at it, and a pass
// that republished them before it would republish them onto the old address.
func (a *App) convergeRecordsLocked(ctx context.Context, moved bool) (int, error) {
	journal, err := a.readDNSJournal()
	if err != nil {
		return 0, fmt.Errorf("无法读取本地托管记录")
	}
	var failures []error
	applied, err := a.appliedSnapshotLocked()
	if err != nil {
		return 0, err
	}
	if applied.AppliedPublicAddress != "" && a.engine != nil {
		if err := a.engine.VerifyApplied(applied, applied.AppliedPublicAddress); err != nil {
			return 0, fmt.Errorf("running proxy proof failed; DNS maintenance paused: %w", err)
		}
	}
	// Include failed creates, which have an attempt but no managed record yet.
	// Recover per name in the same base-first order as maintenance: a slow or
	// unresolved business-domain replacement must not hold the base station up.
	known := make(map[string]bool, len(journal.Managed))
	for host := range journal.Managed {
		known[host] = true
	}
	if journal.Attempt != nil {
		known[journal.Attempt.Host] = true
	}
	for _, attempt := range journal.Attempts {
		known[attempt.Host] = true
	}
	hosts := make([]string, 0, len(known))
	for host := range known {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	hosts = baseFirst(hosts, applied.Settings.BaseDomain)
	published := 0
	for _, host := range hosts {
		if err := ctx.Err(); err != nil {
			return published, errors.Join(append(failures, err)...)
		}
		if err := a.dnsMigrationBlocksHost(host); err != nil {
			failures = append(failures, err)
			continue
		}
		if blocked, err := a.deletionBlocksHost(host); err != nil {
			return published, err
		} else if blocked {
			continue
		}
		// Each name may save a new journal. Never recover the next attempt from
		// the stale snapshot taken before those writes, which would erase them.
		journal, err = a.readDNSJournal()
		if err != nil {
			return published, fmt.Errorf("无法读取本地托管记录")
		}
		unresolved := false
		for attempt := dnsAttemptForHost(journal, host); attempt != nil; attempt = dnsAttemptForHost(journal, host) {
			selectDNSAttempt(&journal, attempt.Host, attempt.AccountID, attempt.Zone)
			if err := a.recoverDNSAttemptLocked(ctx, &journal); err != nil {
				failures = append(failures, fmt.Errorf("自动解析 %s：%w", host, err))
				unresolved = true
				break
			}
		}
		if unresolved {
			continue
		}
		entry, managed := journal.Managed[host]
		if !managed {
			continue
		}
		// Legacy ledgers had no explicit base flag. A published CNAME for an
		// applied service is evidence of its already-authorized base dependency.
		if !entry.Base {
			for _, service := range applied.Services {
				if !service.Enabled {
					continue
				}
				for _, name := range service.Domains {
					if follower, ok := journal.Managed[strings.ToLower(name)]; ok && follower.Record.Type == "CNAME" && sameRecordValue("CNAME", follower.Record.Value, host) {
						entry.Base = true
					}
				}
			}
		}
		desired, maintain := a.maintainedDNSDesired(applied, entry)
		if !maintain {
			continue
		}
		_, changed, err := a.convergeDesiredLocked(ctx, desired)
		if err != nil {
			failures = append(failures, fmt.Errorf("自动解析 %s：%w", host, err))
			continue
		}
		if changed {
			published++
		}
	}
	return published, errors.Join(failures...)
}

func (a *App) recoverDNSAttemptLocked(ctx context.Context, journal *dnsJournal) error {
	if journal.Attempt == nil {
		return nil
	}
	attempt := journal.Attempt
	account := a.accountByID(attempt.AccountID)
	if account.ID == "" || !account.Enabled {
		return fmt.Errorf("待核对 DNS 写入的账号不可用，已停止自动解析")
	}
	sub, err := labelInZone(attempt.Host, attempt.Zone)
	if err != nil {
		return err
	}
	credentials, err := a.accountCredentials(account.ID)
	if err != nil {
		return fmt.Errorf("待核对 DNS 写入的账号凭据不可用")
	}
	records, err := a.dnsAPI(credentials).List(ctx, attempt.Zone, sub)
	if err != nil {
		return fmt.Errorf("只读核对域名 %s 的上次写入失败：%w", attempt.Host, err)
	}
	return a.reconcileDNS(journal, records, dnsDesired{Host: attempt.Host, Zone: attempt.Zone, Account: account})
}

// baseFirst moves one name to the front of a list, keeping every other name in
// the order it arrived in.
//
// The name it moves is the base station, and it goes first because every
// directly published name points at it: a pass that republished them before it
// would republish them onto the address the base station is about to stop
// naming. The order is therefore part of the requirement rather than a
// convenience, which is why it lives in one function — it had already been
// written once here, and a second copy is how two passes come to disagree about
// which name has to be right first.
//
// Sorting is stable, so the rest of the order — and therefore the point at which
// a failure stops the pass — stays reproducible.
func baseFirst(hosts []string, base string) []string {
	if base == "" {
		return hosts
	}
	ordered := make([]string, 0, len(hosts))
	for _, host := range hosts {
		if host == base {
			ordered = append(ordered, host)
		}
	}
	for _, host := range hosts {
		if host != base {
			ordered = append(ordered, host)
		}
	}
	return ordered
}

// convergeHostLocked brings one name in line with what is being served, and
// reports what it did. Callers hold dnsMu and mu.
//
// The action is returned rather than left for the caller to work out: "did
// anything change" and "what happened" are different questions, and the result
// list the operator reads answers the second. Deriving it outside would mean
// reading the provider again to learn something this function already knew, and
// would be a second opinion about the same name.
func (a *App) convergeHostLocked(ctx context.Context, host string) (string, bool, error) {
	desired, err := a.desiredForHostLocked(ctx, host, false)
	if err != nil {
		return "", false, err
	}
	return a.convergeDesiredLocked(ctx, desired)
}

func (a *App) convergeDesiredLocked(ctx context.Context, desired dnsDesired) (string, bool, error) {
	if desired.External {
		// The gateway is letting go of this name. There is no account to read and
		// no cloud call to make, and refusing here would leave the name — and
		// every name queued behind it — stuck forever on a record that belongs to
		// whatever now answers for it.
		j, err := a.readDNSJournal()
		if err != nil {
			return "", false, fmt.Errorf("无法读取本地托管记录")
		}
		plan, err := planDNS(j, desired, nil)
		if err != nil {
			return "", false, err
		}
		if plan.Action == dnsNoop {
			return dnsNoop, false, nil
		}
		if err := a.releaseDNS(&j, desired); err != nil {
			return "", false, err
		}
		return plan.Action, true, nil
	}
	if desired.Account.ID == "" {
		return "", false, fmt.Errorf("该域名对应的 DNS 账号已被删除")
	}
	if desired.Enabled && desired.Value == "" {
		return "", false, nil
	}
	credentials, err := a.accountCredentials(desired.Account.ID)
	if err != nil {
		return "", false, fmt.Errorf("账号凭据不可用")
	}
	j, err := a.readDNSJournal()
	if err != nil {
		return "", false, fmt.Errorf("无法读取本地托管记录")
	}
	client := a.dnsAPI(credentials)
	records, err := client.List(ctx, desired.Zone, desired.Sub)
	if err != nil {
		return "", false, fmt.Errorf("读取云端记录失败：%w", err)
	}
	if err := a.reconcileDNS(&j, records, desired); err != nil {
		return "", false, err
	}
	plan, err := planDNS(j, desired, records)
	if err != nil {
		return "", false, err
	}
	if plan.Action == dnsNoop {
		return dnsNoop, false, nil
	}
	if err := a.publish(ctx, client, &j, desired, plan); err != nil {
		return "", false, err
	}
	return plan.Action, true, nil
}

// publicAddressSummary is the read-only view the interface uses to explain what
// the automatic path is looking at, without performing any part of it.
func (a *App) publicAddressSummary(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	autoResolve := a.state.Settings.AutoResolve
	applied := a.state.AppliedPublicAddress
	base := a.baseDomain()
	followers := 0
	hosts := []string{}
	for _, service := range a.state.Services {
		// An EdgeOne-fronted name does not follow this address, so counting it as
		// a follower would promise a repair that deliberately will not happen.
		if service.Enabled && service.UsesDomains() && !service.ServesEdge() {
			if base != "" {
				// Under a base station every direct name points at one record, so
				// the count of writes is one and the count of names is not what the
				// operator needs to see.
				continue
			}
			followers += len(service.Domains)
			hosts = append(hosts, service.Domains...)
		}
	}
	if base != "" {
		hosts = append([]string{base}, hosts...)
		followers = 1
	}
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	candidates, routes, warnings := a.inspectHost(ctx)
	eligible := []string{}
	matched := false
	for _, candidate := range candidates {
		if candidate.Eligible {
			eligible = append(eligible, candidate.Address)
			if applied != "" && strings.EqualFold(candidate.Address, applied) {
				matched = true
			}
		}
	}
	reply(w, 200, map[string]any{
		"autoResolve": autoResolve, "candidates": candidates, "eligible": eligible,
		"routes": routes, "appliedAddress": applied, "domains": hosts,
		"followers": followers, "matched": matched, "warnings": warnings,
	})
}
