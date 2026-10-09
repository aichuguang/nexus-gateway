package engine

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// OriginProofKey is device-local and never returned by a public API.
func OriginProofKey(dataDir string) (string, error) {
	path := filepath.Join(dataDir, "origin-proof.key")
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() != 64 {
		return "", errors.New("invalid origin proof key")
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if _, err = hex.DecodeString(string(key)); err != nil {
		return "", errors.New("invalid origin proof key")
	}
	return string(key), nil
}

func prepareOriginProofKey(dataDir string) (string, error) {
	key, err := OriginProofKey(dataDir)
	if err == nil || !os.IsNotExist(err) {
		return key, err
	}
	bytes := make([]byte, 32)
	if _, err = rand.Read(bytes); err != nil {
		return "", err
	}
	key = hex.EncodeToString(bytes)
	if err = atomicWrite(filepath.Join(dataDir, "origin-proof.key"), []byte(key)); err != nil {
		return "", err
	}
	return key, nil
}

func OriginProof(key, host, nonce string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(strings.ToLower(host) + "\n" + nonce))
	return hex.EncodeToString(mac.Sum(nil))
}
