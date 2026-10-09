package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"w103d/gateway/internal/atomicfile"
	"w103d/gateway/internal/engine"
	"w103d/gateway/internal/model"
)

const runtimeTransactionFile = "runtime-transaction.json"

// A done record is a durable tombstone: unlike unlinking a journal, an old
// prepared record cannot reappear after a crash and undo subsequent edits.
type runtimeTransaction struct {
	Version        int          `json:"version"`
	Phase          string       `json:"phase"`
	AppliedExisted bool         `json:"appliedExisted,omitempty"`
	Draft          *model.State `json:"previousDraft,omitempty"`
	Applied        *model.State `json:"previousApplied,omitempty"`
	NextDraft      *model.State `json:"nextDraft,omitempty"`
	NextApplied    *model.State `json:"nextApplied,omitempty"`
}

func writeRuntimeJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if (filepath.Base(path) == "state.json" || filepath.Base(path) == "applied.json") && len(b)+1 > 2<<20 {
		return errors.New("runtime snapshot exceeds the 2 MiB readable limit")
	}
	if filepath.Base(path) == runtimeTransactionFile && len(b)+1 > 16<<20 {
		return errors.New("runtime transaction exceeds the readable limit")
	}
	return atomicfile.Write(path, append(b, '\n'), atomicfile.Options{CreateParents: true})
}

func readRuntimeTransaction(dataDir string) (runtimeTransaction, error) {
	var tx runtimeTransaction
	p := filepath.Join(dataDir, runtimeTransactionFile)
	info, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return tx, nil
	}
	if err != nil {
		return tx, err
	}
	if !info.Mode().IsRegular() || info.Size() > 16<<20 {
		return tx, errors.New("invalid runtime transaction file")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return tx, err
	}
	if err = json.Unmarshal(b, &tx); err != nil {
		return tx, err
	}
	if tx.Version != 1 {
		return tx, errors.New("unsupported runtime transaction")
	}
	if tx.Phase == "done" {
		return tx, nil
	}
	if tx.Phase != "prepared" && tx.Phase != "repair-required" {
		return tx, errors.New("invalid runtime transaction phase")
	}
	if tx.Draft == nil || tx.Applied == nil || tx.NextDraft == nil || tx.NextApplied == nil {
		return tx, errors.New("incomplete runtime transaction")
	}
	return tx, nil
}

// RecoverRuntimeState must run under AcquireRuntimeLock, before render or serve.
// Uncommitted work always returns to its complete old snapshot. Render then
// reconstructs the old config before nginx starts; live recovery is forbidden.
func RecoverRuntimeState(dataDir string) error {
	tx, err := readRuntimeTransaction(dataDir)
	if err != nil {
		return err
	}
	if tx.Phase == "" || tx.Phase == "done" {
		return nil
	}
	if alive, e := runtimeProxyAlive(dataDir); e != nil {
		return e
	} else if alive {
		return errors.New("stop the managed proxy process before runtime recovery")
	}
	if conn, e := net.DialTimeout("tcp", "127.0.0.1:18079", 250*time.Millisecond); e == nil {
		_ = conn.Close()
		return errors.New("stop the managed proxy before runtime recovery")
	}
	if err = restoreRuntimeApplied(dataDir, tx.Applied, tx.AppliedExisted, writeRuntimeJSON); err != nil {
		return fmt.Errorf("runtime recovery applied: %w", err)
	}
	if err = writeRuntimeJSON(filepath.Join(dataDir, "state.json"), tx.Draft); err != nil {
		return fmt.Errorf("runtime recovery draft: %w", err)
	}
	return writeRuntimeJSON(filepath.Join(dataDir, runtimeTransactionFile), runtimeTransaction{Version: 1, Phase: "done"})
}

func restoreRuntimeApplied(dataDir string, state *model.State, existed bool, write func(string, any) error) error {
	p := filepath.Join(dataDir, "applied.json")
	if existed {
		return write(p, *state)
	}
	if err := os.Remove(p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return syncRuntimeDirectory(dataDir)
}

func (a *App) commitRuntimeLocked(nextDraft, nextApplied model.State, write func(string, any) error) error {
	prior, err := readRuntimeTransaction(a.opts.DataDir)
	if err != nil {
		return err
	}
	if prior.Phase != "" && prior.Phase != "done" {
		return errApplyNeedsReview
	}
	previous, err := AppliedState(a.opts)
	if err != nil {
		return err
	}
	oldDraft := clone(a.state)
	nextApplied.AppliedRevision = nextApplied.Revision
	nextApplied.AppliedAt = time.Now().UTC().Format(time.RFC3339Nano)
	nextDraft.AppliedRevision = nextApplied.AppliedRevision
	nextDraft.AppliedAt = nextApplied.AppliedAt
	nextDraft.AppliedPublicAddress = nextApplied.AppliedPublicAddress
	_, appliedStatErr := os.Stat(filepath.Join(a.opts.DataDir, "applied.json"))
	if appliedStatErr != nil && !errors.Is(appliedStatErr, os.ErrNotExist) {
		return appliedStatErr
	}
	tx := runtimeTransaction{Version: 1, Phase: "prepared", AppliedExisted: appliedStatErr == nil, Draft: &oldDraft, Applied: &previous, NextDraft: &nextDraft, NextApplied: &nextApplied}
	journal := filepath.Join(a.opts.DataDir, runtimeTransactionFile)
	if err = writeRuntimeJSON(journal, tx); err != nil {
		// A committed-but-unsynced intent cannot safely be treated as absent.
		if atomicfile.IsCommitted(err) {
			a.beginShutdown()
		}
		return err
	}
	snapshotsTouched := false
	rollback := func(cause error) error {
		failures := []error{cause}
		if _, e := a.engine.Apply(previous, previous.AppliedPublicAddress); e != nil {
			failures = append(failures, e)
		} else if e = a.verifyTLS(previous, previous.AppliedPublicAddress); e != nil {
			failures = append(failures, e)
		}
		if snapshotsTouched {
			if e := restoreRuntimeApplied(a.opts.DataDir, &previous, tx.AppliedExisted, write); e != nil {
				failures = append(failures, e)
			}
			if e := write(filepath.Join(a.opts.DataDir, "state.json"), oldDraft); e != nil {
				failures = append(failures, e)
			}
		}
		a.state = oldDraft
		if len(failures) == 1 {
			if e := writeRuntimeJSON(journal, runtimeTransaction{Version: 1, Phase: "done"}); e == nil {
				return fmt.Errorf("%w；已恢复之前代理配置", cause)
			} else {
				failures = append(failures, e)
			}
		}
		tx.Phase = "repair-required"
		if e := writeRuntimeJSON(journal, tx); e != nil {
			failures = append(failures, e)
		}
		a.beginShutdown()
		return fmt.Errorf("%w: %w", errApplyNeedsReview, errors.Join(failures...))
	}
	if _, err = a.engine.Apply(nextApplied, nextApplied.AppliedPublicAddress); err != nil {
		return rollback(err)
	}
	if err = a.verifyTLS(nextApplied, nextApplied.AppliedPublicAddress); err != nil {
		return rollback(err)
	}
	snapshotsTouched = true
	if err = write(filepath.Join(a.opts.DataDir, "applied.json"), nextApplied); err != nil {
		return rollback(err)
	}
	if err = write(filepath.Join(a.opts.DataDir, "state.json"), nextDraft); err != nil {
		return rollback(err)
	}
	if err = writeRuntimeJSON(journal, runtimeTransaction{Version: 1, Phase: "done"}); err != nil {
		if atomicfile.IsCommitted(err) {
			a.state = nextDraft
			a.beginShutdown()
			return fmt.Errorf("runtime committed; durability requires offline verification: %w", err)
		}
		return rollback(err)
	}
	a.state = nextDraft
	return nil
}

func (a *App) maintainAppliedAddressLocked(ctx context.Context, address string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	applied, err := a.appliedSnapshotLocked()
	if err != nil {
		return err
	}
	if applied.AppliedRevision == 0 {
		return errors.New("no applied generation to maintain")
	}
	applied.AppliedPublicAddress = address
	return a.commitRuntimeLocked(clone(a.state), applied, writeRuntimeJSON)
}

func storeCertificateVersion(dataDir string, certificate *model.Certificate, cert, key []byte) error {
	h := sha256.New()
	_, _ = h.Write(cert)
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(key)
	certificate.MaterialVersion = hex.EncodeToString(h.Sum(nil))
	dir, err := engine.CertificateDirectory(dataDir, *certificate)
	if err != nil {
		return err
	}
	for _, file := range []struct {
		name string
		data []byte
	}{{"fullchain.pem", cert}, {"privkey.pem", key}} {
		p := filepath.Join(dir, file.name)
		old, e := os.ReadFile(p)
		if e == nil {
			if !bytes.Equal(old, file.data) {
				return errors.New("immutable certificate material mismatch")
			}
			continue
		}
		if !errors.Is(e, os.ErrNotExist) {
			return e
		}
		if e = atomicfile.Write(p, file.data, atomicfile.Options{CreateParents: true}); e != nil {
			return e
		}
	}
	for p := dir; ; p = filepath.Dir(p) {
		if err := syncRuntimeDirectory(p); err != nil {
			return err
		}
		if filepath.Clean(p) == filepath.Clean(dataDir) {
			break
		}
		if filepath.Dir(p) == p {
			return errors.New("certificate directory escaped data directory")
		}
	}
	return nil
}
