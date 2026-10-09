package app

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const cookieName = "w103d_gateway_session"

type loginAttempt struct {
	Count int
	Until time.Time
}

func randomToken() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func digest(s string) string { b := sha256.Sum256([]byte(s)); return hex.EncodeToString(b[:]) }

func (a *App) initAuth() error {
	p := filepath.Join(a.opts.DataDir, "password.sha256")
	b, e := readAuthFile(p)
	if errors.Is(e, os.ErrNotExist) {
		initial := filepath.Join(a.opts.DataDir, "initial-password")
		b, e = readAuthFile(initial)
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return errors.New("无法读取初始密码文件")
		}
		password := strings.TrimSpace(string(b))
		if errors.Is(e, os.ErrNotExist) {
			switch {
			case a.opts.InitialKey != "":
				if e := validateInitialKey(a.opts.InitialKey); e != nil {
					return e
				}
				password = a.opts.InitialKey
			case a.opts.RequireInitialKey:
				return errors.New("请在config.json填写initialKey")
			default:
				password = randomToken()
				if e = AtomicWrite(initial, []byte(password+"\n")); e != nil {
					return e
				}
			}
		} else if password == "" {
			return errors.New("初始密码文件为空，已停止初始化")
		}
		if len(password) > maxPasswordBytes || !utf8.ValidString(password) {
			return errors.New("初始密码必须是有效 UTF-8 文本，且不超过 512 字节")
		}
		a.passwordHash, e = encodePassword(password)
		if e != nil {
			return e
		}
		return AtomicWrite(p, []byte(a.passwordHash+"\n"))
	}
	if e != nil {
		return e
	}
	a.passwordHash = strings.TrimSpace(string(b))
	if !validPasswordHash(a.passwordHash) {
		return errors.New("密码哈希文件不合法")
	}
	// A crash between committing a changed password and removing its old
	// bootstrap file must not leave an obsolete plaintext secret indefinitely.
	if strings.HasPrefix(a.passwordHash, "$argon2id$") {
		initial := filepath.Join(a.opts.DataDir, "initial-password")
		if b, err := readAuthFile(initial); err == nil {
			if !passwordMatches(strings.TrimSpace(string(b)), a.passwordHash) {
				if err := os.Remove(initial); err != nil {
					return errors.New("无法清理已失效的初始密码文件，请检查数据目录权限")
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return errors.New("无法检查初始密码文件")
		}
	}
	return nil
}

func validateInitialKey(key string) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("请在config.json填写initialKey")
	}
	placeholder := strings.ToUpper(strings.TrimSpace(key))
	if strings.HasPrefix(placeholder, "CHANGE_ME") || strings.HasPrefix(placeholder, "YOUR_INITIAL_KEY") {
		return errors.New("请将config.json的initialKey占位符替换为自己的管理密码")
	}
	count := utf8.RuneCountInString(key)
	if !utf8.ValidString(key) || count < 12 || count > 128 || len(key) > maxPasswordBytes {
		return errors.New("config.json的initialKey需为12–128个字符，且不超过512个UTF-8字节")
	}
	return nil
}

func (a *App) authenticated(r *http.Request) bool {
	a.authMu.Lock()
	defer a.authMu.Unlock()
	return a.authenticatedLocked(r)
}

// Security handlers recheck while holding authMu so a request that passed the
// middleware cannot revoke newer sessions after its own session was revoked.
func (a *App) authenticatedLocked(r *http.Request) bool {
	key := sessionKey(r)
	expiry, ok := a.sessions[key]
	if !ok {
		return false
	}
	if !time.Now().Before(expiry) {
		delete(a.sessions, key)
		delete(a.sessionMeta, key)
		return false
	}
	a.touchSessionLocked(key, r, expiry)
	return true
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	a.authMu.Lock()
	if !a.requirePasswordLocked(w, r, in.Password, http.StatusUnauthorized) {
		a.authMu.Unlock()
		return
	}
	a.expireSecuritySessionsLocked()
	if len(a.sessions) >= 100 {
		a.authMu.Unlock()
		fail(w, 429, "有效会话已达上限，请先退出其他会话")
		return
	}
	if a.sessions == nil {
		a.sessions = map[string]time.Time{}
	}
	token := randomToken()
	key, expiry := digest(token), time.Now().Add(12*time.Hour)
	a.sessions[key] = expiry
	a.touchSessionLocked(key, r, expiry)
	a.authMu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: 43200})
	reply(w, 200, map[string]any{"ok": true})
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if c, e := r.Cookie(cookieName); e == nil {
		a.authMu.Lock()
		delete(a.sessions, digest(c.Value))
		delete(a.sessionMeta, digest(c.Value))
		a.authMu.Unlock()
	}
	clearSessionCookie(w, r)
	reply(w, 200, map[string]bool{"ok": true})
}
