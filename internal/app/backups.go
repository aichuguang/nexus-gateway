package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
	"w103d/gateway/internal/buildinfo"
	"w103d/gateway/internal/engine"
	"w103d/gateway/internal/model"
)

const backupMaxBytes = 64 << 20
const backupFileMaxBytes = 4 << 20

type backupHeader struct {
	Format         string `json:"format"`
	Version        int    `json:"version"`
	ProductVersion string `json:"productVersion"`
	CreatedAt      string `json:"createdAt"`
	Cipher         string `json:"cipher"`
	KDF            string `json:"kdf"`
	MemoryKiB      uint32 `json:"memoryKiB"`
	Iterations     uint32 `json:"iterations"`
	Parallelism    uint8  `json:"parallelism"`
	Salt           string `json:"salt"`
	Nonce          string `json:"nonce"`
}
type backupEnvelope struct {
	Header     backupHeader `json:"header"`
	Ciphertext string       `json:"ciphertext"`
}
type backupMember struct {
	Path   string `json:"path"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}
type backupSource struct {
	path     string
	required bool
}

func backupSources(state model.State) []backupSource {
	files := []backupSource{{"state.json", true}, {"applied.json", state.AppliedRevision > 0}, {"dns-managed.json", false},
		{"publish-jobs.json", false}, {"service-deletions.json", false}, {"config-history.json", false}, {"monitor-settings.json", false}, {"notifications.json", false},
		{"external-probes.json", false}, {"operation-receipt.key", false}, {"origin-proof.key", false}, {"profile.json", false}}
	for _, c := range state.Certificates {
		dir, err := engine.CertificateDirectory(".", c)
		if err != nil {
			dir = "invalid-certificate-reference"
		}
		dir = filepath.ToSlash(dir)
		files = append(files, backupSource{dir + "/fullchain.pem", c.Ready}, backupSource{dir + "/privkey.pem", c.Ready})
	}
	for _, account := range state.DNSAccounts {
		files = append(files, backupSource{"dns-accounts/" + credentialFilename(account.ID, account.CredentialFile), true})
	}
	for _, account := range state.EdgeOneAccounts {
		files = append(files, backupSource{"edgeone-accounts/" + credentialFilename(account.ID, account.CredentialFile), true})
	}
	for _, account := range state.CloudflareAccounts {
		files = append(files, backupSource{"cloudflare-accounts/" + credentialFilename(account.ID, account.CredentialFile), true})
	}
	files = append(files, backupSource{"cloudflare-ledger.json", false}, backupSource{"dns-verification.json", false})
	files = append(files, backupSource{"cloudflare-dcv-delegations.json", false})
	files = append(files, backupSource{"cloudflare-http-validations.json", false})
	return files
}

// Only a fixed allowlist is read. No recursive walk can sweep in password files,
// pending certd inbox data, logs, sockets or unrelated secrets from this directory.
func readBackupFile(root, relative string) ([]byte, error) {
	if !filepath.IsAbs(root) || filepath.IsAbs(relative) || strings.Contains(relative, "\\") {
		return nil, errors.New("备份路径无效")
	}
	parts := strings.Split(relative, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, errors.New("备份路径无效")
		}
	}
	path := root
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("备份目录不可用或是符号链接")
	}
	for i, part := range parts {
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || (i < len(parts)-1 && !info.IsDir()) {
			return nil, errors.New("备份不接受符号链接或异常目录")
		}
		if i == len(parts)-1 {
			if !info.Mode().IsRegular() || info.Size() > backupFileMaxBytes {
				return nil, errors.New("备份文件异常或超过 4 MiB")
			}
			file, err := os.Open(path)
			if err != nil {
				return nil, err
			}
			defer file.Close()
			opened, err := file.Stat()
			if err != nil || !os.SameFile(info, opened) {
				return nil, errors.New("备份文件在读取时发生变化")
			}
			data, err := io.ReadAll(io.LimitReader(file, backupFileMaxBytes+1))
			if err != nil || len(data) > backupFileMaxBytes {
				return nil, errors.New("无法完整读取备份文件")
			}
			return data, nil
		}
	}
	return nil, errors.New("备份文件不存在")
}

func (a *App) backupArchiveLocked() ([]byte, error) {
	tx, err := readRuntimeTransaction(a.opts.DataDir)
	if err != nil {
		return nil, err
	}
	if tx.Phase != "" && tx.Phase != "done" {
		return nil, errApplyNeedsReview
	}
	applied, err := AppliedState(a.opts)
	if err != nil {
		return nil, err
	}
	sources := backupSources(a.state)
	// Running references may differ from an unfinished draft (including removed
	// slots). A configuration backup must preserve both sides of that boundary.
	for _, c := range applied.Certificates {
		dir, err := engine.CertificateDirectory(".", c)
		if err != nil {
			return nil, err
		}
		for _, name := range []string{"fullchain.pem", "privkey.pem"} {
			sources = append(sources, backupSource{filepath.ToSlash(filepath.Join(dir, name)), c.Ready})
		}
	}
	for _, ledgerDir := range []string{"operation-receipts", "dns-migrations", "cloud-operation-history", "dns-txt-cleanup", "dns-txt-archive"} {
		receiptDir := filepath.Join(a.opts.DataDir, ledgerDir)
		if st, e := os.Lstat(receiptDir); e == nil && (!st.IsDir() || st.Mode()&os.ModeSymlink != 0) {
			return nil, errors.New("invalid operation receipt directory")
		}
		entries, err := os.ReadDir(receiptDir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".write-") && !entry.IsDir() {
				continue
			}
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(entry.Name(), ".json") {
				return nil, errors.New("unexpected operation receipt member")
			}
			sources = append(sources, backupSource{ledgerDir + "/" + entry.Name(), true})
		}
	}
	var buffer bytes.Buffer
	zipped := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(zipped)
	defer zipped.Close()
	defer archive.Close()
	members := []backupMember{}
	total := 0
	seen := map[string]bool{}
	for _, source := range sources {
		if seen[source.path] {
			continue
		}
		seen[source.path] = true
		data, err := readBackupFile(a.opts.DataDir, source.path)
		if errors.Is(err, os.ErrNotExist) && !source.required {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("无法备份 %s：%w", source.path, err)
		}
		if source.path == "dns-managed.json" || source.path == "cloudflare-ledger.json" || source.path == "dns-verification.json" {
			data, err = materializeCloudLedger(filepath.Join(a.opts.DataDir, source.path), data)
			if err != nil {
				return nil, err
			}
		}
		if len(data) > backupFileMaxBytes {
			clear(data)
			return nil, fmt.Errorf("展开后的备份成员 %s 超过 4 MiB，请使用完整设备备份", source.path)
		}
		total += len(data)
		if total > backupMaxBytes {
			clear(data)
			return nil, errors.New("备份内容超过 64 MiB 上限")
		}
		sum := sha256.Sum256(data)
		members = append(members, backupMember{source.path, len(data), hex.EncodeToString(sum[:])})
		if err = archive.WriteHeader(&tar.Header{Name: source.path, Mode: 0600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err == nil {
			_, err = archive.Write(data)
		}
		clear(data)
		if err != nil {
			return nil, errors.New("无法创建备份内容")
		}
	}
	manifest, err := json.Marshal(map[string]any{"format": 1, "productVersion": buildinfo.Version, "schemaVersion": model.SchemaVersion,
		"scope": "configuration-and-operation-evidence", "restorePolicy": "offline validation required; never resubmit uncertain operations automatically",
		"createdAt": time.Now().UTC().Format(time.RFC3339), "files": members,
		"excluded": []string{"management password", "sessions", "cert-inbox", "logs", "traffic counters", "systemd/env files", "cache"}})
	if err != nil {
		return nil, err
	}
	if err = archive.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0600, Size: int64(len(manifest)), Typeflag: tar.TypeReg}); err == nil {
		_, err = archive.Write(manifest)
	}
	if err != nil {
		return nil, err
	}
	if err = archive.Close(); err != nil {
		return nil, err
	}
	if err = zipped.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// AES-GCM authenticates both the archive and all format/KDF parameters. The
// archive and derived key never touch disk. This is export, not a restore API.
func encryptBackup(archive []byte, passphrase string) ([]byte, error) {
	salt, nonce := make([]byte, 16), make([]byte, 12)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	header := backupHeader{Format: "W103D-BACKUP", Version: 1, ProductVersion: buildinfo.Version, CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Cipher: "AES-256-GCM", KDF: "argon2id", MemoryKiB: 64 * 1024, Iterations: 3, Parallelism: 1,
		Salt: base64.StdEncoding.EncodeToString(salt), Nonce: base64.StdEncoding.EncodeToString(nonce)}
	key := argon2.IDKey([]byte(passphrase), salt, header.Iterations, header.MemoryKiB, header.Parallelism, 32)
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	aad, err := json.Marshal(header)
	if err != nil {
		return nil, err
	}
	sealed := gcm.Seal(nil, nonce, archive, aad)
	return json.Marshal(backupEnvelope{Header: header, Ciphertext: base64.StdEncoding.EncodeToString(sealed)})
}

func (a *App) downloadBackup(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password          string `json:"password"`
		Passphrase        string `json:"passphrase"`
		ConfirmPassphrase string `json:"confirmPassphrase"`
	}
	if !decode(w, r, &in) {
		return
	}
	length := utf8.RuneCountInString(in.Passphrase)
	if !utf8.ValidString(in.Passphrase) || length < 12 || length > 128 || len(in.Passphrase) > 512 || in.Passphrase != in.ConfirmPassphrase {
		fail(w, 400, "备份口令需为 12–128 个字符且两次输入一致")
		return
	}
	if !a.requirePassword(w, r, in.Password) {
		return
	}
	a.mu.Lock()
	archive, err := a.backupArchiveLocked()
	a.mu.Unlock()
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	defer clear(archive)
	if r.Context().Err() != nil {
		return
	}
	data, err := encryptBackup(archive, in.Passphrase)
	if err != nil {
		fail(w, 500, "无法加密备份，未生成下载文件")
		return
	}
	if r.Context().Err() != nil {
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", `attachment; filename="gateway-backup-`+time.Now().UTC().Format("20060102T150405Z")+`.enc"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
