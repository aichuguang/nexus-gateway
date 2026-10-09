package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Account/hosting migration is no longer supported. Read old ledgers only to
// protect records and credentials whose ownership may still be unresolved.
// Nothing here creates, updates, retires, or deletes a cloud record or ledger.
type legacyDNSMigration struct {
	ID            string     `json:"id"`
	Host          string     `json:"host"`
	Phase         string     `json:"phase"`
	Source        dnsManaged `json:"source"`
	Target        dnsManaged `json:"target"`
	SourceRetired bool       `json:"sourceRetired"`
}

func (a *App) legacyDNSMigrations() ([]legacyDNSMigration, error) {
	dir := filepath.Join(a.opts.DataDir, "dns-migrations")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result []legacyDNSMigration
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".write-") && !entry.IsDir() {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(entry.Name(), ".json") || len(id) != 64 || !validID.MatchString(id) {
			return nil, errors.New("invalid legacy DNS migration ledger member")
		}
		var item legacyDNSMigration
		if err := readDNSJSON(filepath.Join(dir, entry.Name()), &item); err != nil {
			return nil, err
		}
		if item.ID != id {
			return nil, errors.New("legacy DNS migration identity mismatch")
		}
		result = append(result, item)
	}
	return result, nil
}

func (a *App) dnsMigrationBlocksHost(host string) error {
	items, err := a.legacyDNSMigrations()
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.Host != host {
			continue
		}
		switch item.Phase {
		case "preview", "switched", "retired", "cancelled", "archived":
			continue
		default:
			return errors.New("legacy DNS ownership is unresolved; verify the retained records with the DNS provider before resuming management; account migration is not supported")
		}
	}
	return nil
}

func (a *App) dnsMigrationUsesAccount(id string) (bool, error) {
	items, err := a.legacyDNSMigrations()
	if err != nil {
		return true, err
	}
	for _, item := range items {
		if item.Phase == "preview" || item.Phase == "archived" {
			continue
		}
		if item.Source.AccountID == id && !item.SourceRetired || item.Target.AccountID == id {
			return true, nil
		}
	}
	return false, nil
}
