package app

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
)

// The manual TXT cleanup feature is removed. Its old write-ahead markers stay
// read-only: an unresolved delete must still prevent automatic TXT recreation.
func (a *App) txtCleanupPath(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(a.opts.DataDir, "dns-txt-cleanup", hex.EncodeToString(sum[:])+".json")
}

func (a *App) txtCleanupUnknown(key string) (bool, error) {
	var marker struct {
		Outcome string `json:"outcome"`
	}
	err := readDNSJSON(a.txtCleanupPath(key), &marker)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	return marker.Outcome != "succeeded" && marker.Outcome != "abandoned-not-observed", nil
}
