package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"w103d/gateway/internal/dnspod"
	"w103d/gateway/internal/model"
)

// Activity and historical ownership share a bounded ledger. Reserve space before
// sending a mutation so a normal provider result cannot cross the read limit.
const cloudLedgerLimit = 256 << 20
const cloudLedgerReserve = 128 << 10
const cloudLedgerShardSize = 1 << 20

type cloudLedgerManifest struct {
	Format     string `json:"format"`
	Generation string `json:"generation"`
	Size       int    `json:"size"`
	SHA256     string `json:"sha256"`
	Parts      int    `json:"parts"`
}

func reserveCloudLedger(value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if len(b) > cloudLedgerLimit-cloudLedgerReserve {
		return errors.New("cloud ledger capacity exhausted; archive verified history before any cloud write")
	}
	return nil
}

func saveCloudLedger(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if len(b) > cloudLedgerLimit {
		return errors.New("cloud ledger exceeds readable capacity")
	}
	if len(b) <= cloudLedgerShardSize {
		return AtomicWrite(path, b)
	}
	var previous cloudLedgerManifest
	if info, e := os.Stat(path); e == nil && info.Size() < 64<<10 {
		if current, e := os.ReadFile(path); e == nil {
			_ = json.Unmarshal(current, &previous)
		}
	}
	generation, err := dnsRandomToken()
	if err != nil {
		return err
	}
	dir := filepath.Join(path+".versions", generation)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	parts := (len(b) + cloudLedgerShardSize - 1) / cloudLedgerShardSize
	for i := 0; i < parts; i++ {
		end := (i + 1) * cloudLedgerShardSize
		if end > len(b) {
			end = len(b)
		}
		if err := AtomicWrite(filepath.Join(dir, fmt.Sprintf("%04d.part", i)), b[i*cloudLedgerShardSize:end]); err != nil {
			return err
		}
	}
	sum := sha256.Sum256(b)
	// The old file remains the authoritative commit until every immutable part
	// is durable. Orphan generations are harmless and can be archived later.
	if err := writeJSON(path, cloudLedgerManifest{Format: "w103d-cloud-ledger-v1", Generation: generation, Size: len(b), SHA256: hex.EncodeToString(sum[:]), Parts: parts}); err != nil {
		return err
	}
	// Keep the just-replaced generation for diagnosis/rollback. Only generated
	// directories under this exact ledger's sibling directory are collectible.
	if entries, e := os.ReadDir(path + ".versions"); e == nil {
		for _, entry := range entries {
			name := entry.Name()
			if !entry.IsDir() || name == generation || name == previous.Generation || len(name) != 64 {
				continue
			}
			if _, e := hex.DecodeString(name); e == nil {
				_ = os.RemoveAll(filepath.Join(path+".versions", name))
			}
		}
	}
	return nil
}

// Also used by backup: export aggregate JSON, never a manifest without its parts.
func materializeCloudLedger(path string, b []byte) ([]byte, error) {
	var m cloudLedgerManifest
	if json.Unmarshal(b, &m) != nil || m.Format == "" {
		return b, nil
	}
	if m.Format != "w103d-cloud-ledger-v1" || m.Size < 1 || m.Size > cloudLedgerLimit || m.Parts != (m.Size+cloudLedgerShardSize-1)/cloudLedgerShardSize {
		return nil, errors.New("invalid cloud ledger manifest")
	}
	if _, err := hex.DecodeString(m.Generation); err != nil || len(m.Generation) < 16 || len(m.Generation) > 128 {
		return nil, errors.New("invalid cloud ledger generation")
	}
	out := make([]byte, 0, m.Size)
	for i := 0; i < m.Parts; i++ {
		part := filepath.Join(path+".versions", m.Generation, fmt.Sprintf("%04d.part", i))
		st, err := os.Lstat(part)
		if err != nil {
			return nil, err
		}
		expected := cloudLedgerShardSize
		if i == m.Parts-1 {
			expected = m.Size - i*cloudLedgerShardSize
		}
		if !st.Mode().IsRegular() || st.Size() != int64(expected) {
			return nil, errors.New("invalid cloud ledger part")
		}
		data, err := os.ReadFile(part)
		if err != nil {
			return nil, err
		}
		out = append(out, data...)
	}
	sum := sha256.Sum256(out)
	if hex.EncodeToString(sum[:]) != m.SHA256 {
		return nil, errors.New("cloud ledger integrity failure")
	}
	return out, nil
}

// All callers hold mu. Account disablement is an immediate control-plane
// restriction, not a value inherited from the applied service snapshot.
func (a *App) authorizeDNSWrite(accountID string) error {
	account := a.accountByID(accountID)
	if account.ID == "" || !account.Enabled {
		return errors.New("DNS account is missing or disabled; cloud writes are prohibited")
	}
	if a.opts.DryRun {
		return errors.New("dry-run prohibits cloud writes")
	}
	return nil
}

// Published records authorize maintenance, not publication of other draft or
// applied names. Never turn a changed entry mode into an implicit CNAME switch.
func (a *App) maintainedDNSDesired(applied model.State, entry dnsManaged) (dnsDesired, bool) {
	d := dnsDesired{Host: entry.Host, Account: a.accountByID(entry.AccountID), Zone: entry.Zone, Base: entry.Base,
		Sub: entry.Record.Name, Enabled: true, Type: entry.Record.Type, Value: entry.Record.Value,
		TTL: entry.Record.TTL, Revision: applied.AppliedRevision}
	if d.Type != dnspod.TypeAAAA && d.Type != dnspod.TypeA {
		return d, false
	}
	served := entry.Base || strings.EqualFold(entry.Host, applied.Settings.BaseDomain)
	for _, service := range applied.Services {
		if !service.Enabled {
			continue
		}
		for _, host := range service.Domains {
			if strings.EqualFold(host, entry.Host) {
				served = true
				d.ServiceID = service.ID
			}
		}
	}
	if !served || applied.AppliedPublicAddress == "" {
		return d, false
	}
	typ, err := addressRecordType(applied.AppliedPublicAddress)
	if err != nil {
		return d, false
	}
	d.Type, d.Value = typ, applied.AppliedPublicAddress
	return d, true
}
