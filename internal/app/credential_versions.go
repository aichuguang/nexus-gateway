package app

import (
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
)

func credentialFilename(id, reference string) string {
	if reference == "" {
		return id + ".json"
	}
	prefix := id + ".v-"
	if !strings.HasPrefix(reference, prefix) || !strings.HasSuffix(reference, ".json") {
		return "invalid-credential-reference"
	}
	version := strings.TrimSuffix(strings.TrimPrefix(reference, prefix), ".json")
	if _, err := hex.DecodeString(version); err != nil || len(version) != 64 {
		return "invalid-credential-reference"
	}
	return reference
}

// Credentials are immutable. A failed state commit only leaves an unreferenced
// version, never mismatched credentials and CredentialVersion for a live account.
func (a *App) stageCredential(kind, id string, value any) (string, error) {
	version, err := dnsRandomToken()
	if err != nil {
		return "", err
	}
	name := id + ".v-" + version + ".json"
	if credentialFilename(id, name) != name {
		return "", errors.New("invalid credential reference")
	}
	if err := writeJSON(filepath.Join(a.opts.DataDir, kind+"-accounts", name), value); err != nil {
		return "", err
	}
	return name, nil
}
