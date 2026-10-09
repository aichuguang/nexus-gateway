package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"w103d/gateway/internal/model"
	"w103d/gateway/internal/statecheck"
)

// MigrateState is an explicit offline command. It never renders a proxy or
// calls a provider. The same durable rollback record used by local apply keeps
// a crash between the two snapshots from creating a mixed schema installation.
func MigrateState(dataDir string) error {
	unlock, err := AcquireRuntimeLock(dataDir)
	if err != nil {
		return err
	}
	defer unlock()
	if conn, err := net.DialTimeout("tcp", "127.0.0.1:18079", 250*time.Millisecond); err == nil {
		conn.Close()
		return errors.New("stop the managed proxy before state-migrate")
	}
	if err := RecoverRuntimeState(dataDir); err != nil {
		return err
	}
	if _, err := statecheck.Inspect(dataDir, true); err != nil {
		return err
	}
	read := func(name string) (model.State, []byte, error) {
		var state model.State
		b, err := os.ReadFile(filepath.Join(dataDir, name))
		if err != nil {
			return state, nil, err
		}
		err = json.Unmarshal(b, &state)
		return state, b, err
	}
	draft, draftBytes, err := read("state.json")
	if err != nil {
		return err
	}
	applied, appliedBytes, err := read("applied.json")
	existed := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if draft.SchemaVersion == model.SchemaVersion && (!existed || applied.SchemaVersion == model.SchemaVersion) {
		return nil
	}
	nextDraft, nextApplied := clone(draft), clone(applied)
	if err = model.UpgradeSchema(&nextDraft); err != nil {
		return err
	}
	if existed {
		if err = model.UpgradeSchema(&nextApplied); err != nil {
			return err
		}
	}
	backup := filepath.Join(dataDir, fmt.Sprintf("schema-backup-%d-%s", draft.SchemaVersion, NewID()))
	if err = AtomicWrite(filepath.Join(backup, "state.json"), draftBytes); err != nil {
		return err
	}
	if existed {
		if err = AtomicWrite(filepath.Join(backup, "applied.json"), appliedBytes); err != nil {
			return err
		}
	}
	tx := runtimeTransaction{Version: 1, Phase: "prepared", AppliedExisted: existed, Draft: &draft, Applied: &applied, NextDraft: &nextDraft, NextApplied: &nextApplied}
	journal := filepath.Join(dataDir, runtimeTransactionFile)
	if err = writeRuntimeJSON(journal, tx); err != nil {
		return err
	}
	if existed {
		if err = writeRuntimeJSON(filepath.Join(dataDir, "applied.json"), nextApplied); err != nil {
			return err
		}
	}
	if err = writeRuntimeJSON(filepath.Join(dataDir, "state.json"), nextDraft); err != nil {
		return err
	}
	return writeRuntimeJSON(journal, runtimeTransaction{Version: 1, Phase: "done"})
}
