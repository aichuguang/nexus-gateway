package app

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Fixed, bounded parameters avoid trusting PHC input to allocate arbitrary
// memory. Hashing is serialized by authMu on this small ARM64 appliance.
const passwordPHCPrefix = "$argon2id$v=19$m=65536,t=3,p=1$"
const maxPasswordBytes = 512

func encodePassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", errors.New("无法生成安全密码盐")
	}
	key := argon2.IDKey([]byte(password), salt, 3, 64*1024, 1, 32)
	return passwordPHCPrefix + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key), nil
}

func decodePasswordHash(encoded string) (salt, key []byte, valid bool) {
	if !strings.HasPrefix(encoded, passwordPHCPrefix) {
		return nil, nil, false
	}
	parts := strings.Split(strings.TrimPrefix(encoded, passwordPHCPrefix), "$")
	if len(parts) != 2 {
		return nil, nil, false
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[0])
	if err != nil || len(salt) != 16 {
		return nil, nil, false
	}
	key, err = base64.RawStdEncoding.Strict().DecodeString(parts[1])
	return salt, key, err == nil && len(key) == 32
}

func validPasswordHash(encoded string) bool {
	if len(encoded) == 64 {
		_, err := hex.DecodeString(encoded)
		return err == nil
	}
	_, _, valid := decodePasswordHash(encoded)
	return valid
}

func passwordMatches(password, encoded string) bool {
	if password == "" || len(password) > maxPasswordBytes || !utf8.ValidString(password) {
		return false
	}
	if len(encoded) == 64 {
		want, err := hex.DecodeString(encoded)
		if err != nil {
			return false
		}
		got := sha256.Sum256([]byte(password))
		return subtle.ConstantTimeCompare(got[:], want) == 1
	}
	salt, want, valid := decodePasswordHash(encoded)
	if !valid {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, 3, 64*1024, 1, 32)
	return subtle.ConstantTimeCompare(got, want) == 1
}

func readAuthFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1024 {
		return nil, errors.New("认证文件必须是小于 1 KiB 的普通文件")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("认证文件在读取时发生变化")
	}
	data, err := io.ReadAll(io.LimitReader(file, 1025))
	if len(data) > 1024 {
		return nil, errors.New("认证文件过大")
	}
	return data, err
}

// Sensitive actions share the login attempt budget and never migrate the legacy
// hash implicitly. Only an explicitly confirmed password change upgrades it.
func (a *App) requirePassword(w http.ResponseWriter, r *http.Request, password string) bool {
	a.authMu.Lock()
	defer a.authMu.Unlock()
	if !a.requireSecuritySessionLocked(w, r) {
		return false
	}
	return a.requirePasswordLocked(w, r, password, http.StatusForbidden)
}

func (a *App) requirePasswordLocked(w http.ResponseWriter, r *http.Request, password string, failureStatus int) bool {
	if r.Context().Err() != nil {
		fail(w, http.StatusServiceUnavailable, "请求已取消，请重新确认操作")
		return false
	}
	now, ip := time.Now(), securityClientIP(r)
	if a.attempts == nil {
		a.attempts = map[string]loginAttempt{}
	}
	for key, value := range a.attempts {
		if !now.Before(value.Until) {
			delete(a.attempts, key)
		}
	}
	attempt := a.attempts[ip]
	if attempt.Count >= 5 || (attempt.Count == 0 && len(a.attempts) >= 1024) {
		w.Header().Set("Retry-After", "300")
		fail(w, http.StatusTooManyRequests, "尝试过多，请五分钟后重试")
		return false
	}
	if !passwordMatches(password, a.passwordHash) {
		if attempt.Count == 0 {
			attempt.Until = now.Add(5 * time.Minute)
		}
		attempt.Count++
		a.attempts[ip] = attempt
		// Failed step-up authentication must not look like an expired session to
		// API clients. Only a failed initial login uses HTTP 401.
		fail(w, failureStatus, "管理密码不正确")
		return false
	}
	if r.Context().Err() != nil {
		fail(w, http.StatusServiceUnavailable, "请求已取消，请重新确认操作")
		return false
	}
	delete(a.attempts, ip)
	return true
}

func (a *App) changeSecurityPassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
		ConfirmPassword string `json:"confirmPassword"`
	}
	if !decode(w, r, &in) {
		return
	}
	count := utf8.RuneCountInString(in.NewPassword)
	if !utf8.ValidString(in.NewPassword) || count < 12 || count > 128 || len(in.NewPassword) > maxPasswordBytes {
		fail(w, 400, "新密码需为 12–128 个字符，且不超过 512 个 UTF-8 字节")
		return
	}
	if in.NewPassword != in.ConfirmPassword {
		fail(w, 400, "两次输入的新密码不一致")
		return
	}
	if in.NewPassword == in.CurrentPassword {
		fail(w, 400, "新密码不能与当前密码相同")
		return
	}
	a.authMu.Lock()
	defer a.authMu.Unlock()
	if !a.requireSecuritySessionLocked(w, r) {
		return
	}
	if !a.requirePasswordLocked(w, r, in.CurrentPassword, http.StatusForbidden) {
		return
	}
	newHash, err := encodePassword(in.NewPassword)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if r.Context().Err() != nil {
		fail(w, http.StatusServiceUnavailable, "请求已取消，密码未修改")
		return
	}
	changed, err := a.replacePasswordLocked(newHash, AtomicWrite, os.Remove)
	if changed {
		a.sessions = map[string]time.Time{}
		a.sessionMeta = map[string]sessionMetadata{}
		clearSessionCookie(w, r)
	}
	if err != nil {
		if changed {
			reply(w, 500, map[string]any{"error": err.Error(), "changed": true, "relogin": true})
		} else {
			fail(w, 503, err.Error())
		}
		return
	}
	reply(w, 200, map[string]bool{"ok": true, "relogin": true})
}

// Initial plaintext is removed only after the hash commit. A failed cleanup
// rolls back the hash; if rollback also fails, memory follows the committed hash
// and the caller revokes sessions rather than claiming nothing changed.
func (a *App) replacePasswordLocked(newHash string, write func(string, []byte) error, remove func(string) error) (bool, error) {
	initial := filepath.Join(a.opts.DataDir, "initial-password")
	if _, err := readAuthFile(initial); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, errors.New("无法安全检查初始密码文件，密码未修改")
	}
	hashPath := filepath.Join(a.opts.DataDir, "password.sha256")
	previous := a.passwordHash
	if err := write(hashPath, []byte(newHash+"\n")); err != nil {
		return false, errors.New("无法保存新密码，密码未修改")
	}
	if err := remove(initial); err != nil && !errors.Is(err, os.ErrNotExist) {
		if rollbackErr := write(hashPath, []byte(previous+"\n")); rollbackErr != nil {
			a.passwordHash = newHash
			return true, errors.New("新密码已生效，但旧初始密码文件清理及回退失败；会话已注销，请使用新密码重新登录，并检查数据目录权限")
		}
		return false, errors.New("初始密码文件无法清理，已恢复原密码；请检查数据目录权限后重试")
	}
	a.passwordHash = newHash
	return true, nil
}
