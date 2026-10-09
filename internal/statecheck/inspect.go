// Package statecheck provides an offline, read-only schema and ledger preflight.
// It does not migrate state, inspect credentials or certificate materials, render
// a proxy configuration, or claim that a running proxy or cloud account is valid.
package statecheck

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"w103d/gateway/internal/model"
)

const MaxFileBytes = 2 << 20

// SnapshotSummary deliberately contains no names, addresses, account hints,
// certificate metadata, or raw JSON values.
type SnapshotSummary struct {
	SchemaVersion   int   `json:"schemaVersion"`
	Revision        int64 `json:"revision"`
	AppliedRevision int64 `json:"appliedRevision"`
	Services        int   `json:"services"`
	EnabledServices int   `json:"enabledServices"`
	Certificates    int   `json:"certificates"`
	DNSAccounts     int   `json:"dnsAccounts"`
	EdgeOneAccounts int   `json:"edgeOneAccounts"`
	EdgeOrigins     int   `json:"edgeOrigins"`
}

type Report struct {
	SupportedSchemaVersion int              `json:"supportedSchemaVersion"`
	State                  SnapshotSummary  `json:"state"`
	Applied                *SnapshotSummary `json:"applied,omitempty"`
	// AppliedStatus is not_applied, current_revision, or older_revision. A
	// revision difference is not a claim that the proxy's served fields differ.
	AppliedStatus string `json:"appliedStatus"`
}

// Inspect requires an existing directory and state.json. A missing applied.json
// is valid only before the first application. Both files must be regular files,
// not symlinks. Run with the service stopped for a stable two-file view: this
// tool does not acquire the live process's mutex or make a transactional snapshot.
func Inspect(dataDir string, legacy ...bool) (Report, error) {
	report := Report{SupportedSchemaVersion: model.SchemaVersion}
	info, err := os.Lstat(dataDir)
	if err != nil {
		return report, fmt.Errorf("data directory is not readable: %w", err)
	}
	if !info.IsDir() {
		return report, errors.New("data directory must be an existing directory, not a symlink")
	}
	if txInfo, err := os.Lstat(filepath.Join(dataDir, "runtime-transaction.json")); err == nil {
		if !txInfo.Mode().IsRegular() || txInfo.Size() > 16<<20 {
			return report, errors.New("invalid runtime transaction journal")
		}
		b, err := os.ReadFile(filepath.Join(dataDir, "runtime-transaction.json"))
		if err != nil {
			return report, err
		}
		var tx struct {
			Version int    `json:"version"`
			Phase   string `json:"phase"`
		}
		if err = json.Unmarshal(b, &tx); err != nil || tx.Version != 1 || tx.Phase != "done" {
			return report, errors.New("runtime transaction requires offline recovery before deployment")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return report, err
	}
	state, err := readSnapshot(dataDir, "state.json", legacy...)
	if err != nil {
		return report, fmt.Errorf("state.json: %w", err)
	}
	report.State = summarize(state)
	applied, err := readSnapshot(dataDir, "applied.json", legacy...)
	if errors.Is(err, os.ErrNotExist) {
		if state.AppliedRevision != 0 {
			return report, errors.New("applied.json is missing although state.json records an applied revision")
		}
		report.AppliedStatus = "not_applied"
		return report, nil
	}
	if err != nil {
		return report, fmt.Errorf("applied.json: %w", err)
	}
	summary := summarize(applied)
	report.Applied = &summary
	if applied.Revision != applied.AppliedRevision || applied.AppliedRevision == 0 {
		return report, errors.New("applied.json must describe a fully applied revision")
	}
	if state.AppliedRevision != applied.AppliedRevision || state.AppliedAt != applied.AppliedAt || state.AppliedPublicAddress != applied.AppliedPublicAddress {
		return report, errors.New("state.json and applied.json disagree about the applied ledger; stop the service and investigate before changing files")
	}
	if state.Settings.GatewayIP != applied.Settings.GatewayIP || state.Settings.CertdURL != applied.Settings.CertdURL {
		return report, errors.New("state.json and applied.json have inconsistent startup settings")
	}
	report.AppliedStatus = "current_revision"
	if state.Revision != applied.Revision {
		report.AppliedStatus = "older_revision"
	}
	return report, nil
}

func readSnapshot(directory, name string, legacy ...bool) (model.State, error) {
	var state model.State
	path := filepath.Join(directory, name)
	info, err := os.Lstat(path)
	if err != nil {
		return state, err
	}
	if !info.Mode().IsRegular() || info.Size() > MaxFileBytes {
		return state, errors.New("must be a regular file no larger than 2 MiB; symlinks are not accepted")
	}
	file, err := os.Open(path)
	if err != nil {
		return state, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return state, errors.New("file changed while opening; retry with the service stopped")
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	if err != nil {
		return state, err
	}
	if len(data) > MaxFileBytes {
		return state, errors.New("file exceeds 2 MiB")
	}
	if err := decodeStrictVersion(data, &state, len(legacy) > 0 && legacy[0]); err != nil {
		return state, err
	}
	if state.Revision < 1 || state.AppliedRevision < 0 || state.AppliedRevision > state.Revision {
		return state, errors.New("revision must be positive and appliedRevision must be between zero and revision")
	}
	if state.AppliedRevision == 0 {
		if state.AppliedAt != "" || state.AppliedPublicAddress != "" {
			return state, errors.New("unapplied state must not contain applied timestamp or address metadata")
		}
	} else if _, err := time.Parse(time.RFC3339, state.AppliedAt); err != nil {
		return state, errors.New("appliedAt must be an RFC3339 timestamp when appliedRevision is nonzero")
	}
	return state, nil
}

func summarize(state model.State) SnapshotSummary {
	enabled := 0
	for _, service := range state.Services {
		if service.Enabled {
			enabled++
		}
	}
	return SnapshotSummary{SchemaVersion: state.SchemaVersion, Revision: state.Revision, AppliedRevision: state.AppliedRevision,
		Services: len(state.Services), EnabledServices: enabled, Certificates: len(state.Certificates),
		DNSAccounts: len(state.DNSAccounts), EdgeOneAccounts: len(state.EdgeOneAccounts), EdgeOrigins: len(state.EdgeOrigins)}
}
