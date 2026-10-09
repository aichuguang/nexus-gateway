package app

import (
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"
)

type sessionMetadata struct {
	ID         string
	CreatedAt  time.Time
	LastSeenAt time.Time
	Client     string
	IP         string
}

type securitySession struct {
	ID         string `json:"id"`
	Current    bool   `json:"current"`
	CreatedAt  string `json:"createdAt"`
	LastSeenAt string `json:"lastSeenAt"`
	ExpiresAt  string `json:"expiresAt"`
	Client     string `json:"client"`
	IP         string `json:"ip"`
}

func securityClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return "unknown"
}

func sessionKey(r *http.Request) string {
	if cookie, err := r.Cookie(cookieName); err == nil {
		return digest(cookie.Value)
	}
	return ""
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true,
		Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: -1})
}

func (a *App) requireSecuritySessionLocked(w http.ResponseWriter, r *http.Request) bool {
	if a.authenticatedLocked(r) {
		return true
	}
	clearSessionCookie(w, r)
	fail(w, http.StatusUnauthorized, "会话已失效，请重新登录管理台")
	return false
}

func (a *App) touchSessionLocked(key string, r *http.Request, expiry time.Time) {
	if a.sessionMeta == nil {
		a.sessionMeta = map[string]sessionMetadata{}
	}
	metadata, exists := a.sessionMeta[key]
	if !exists {
		client := strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, r.UserAgent())
		if runes := []rune(client); len(runes) > 160 {
			client = string(runes[:160])
		}
		metadata = sessionMetadata{ID: randomToken(), CreatedAt: time.Now(), Client: client, IP: securityClientIP(r)}
		// Legacy in-memory test/session entries have no metadata. The expiry is
		// still authoritative; never extend their lifetime while describing them.
		if metadata.CreatedAt.After(expiry) {
			metadata.CreatedAt = expiry
		}
	}
	metadata.LastSeenAt = time.Now()
	a.sessionMeta[key] = metadata
}

func (a *App) expireSecuritySessionsLocked() {
	now := time.Now()
	for key, expiry := range a.sessions {
		if !now.Before(expiry) {
			delete(a.sessions, key)
			delete(a.sessionMeta, key)
		}
	}
}

func (a *App) listSecuritySessions(w http.ResponseWriter, r *http.Request) {
	a.authMu.Lock()
	defer a.authMu.Unlock()
	if !a.requireSecuritySessionLocked(w, r) {
		return
	}
	a.expireSecuritySessionsLocked()
	current := sessionKey(r)
	result := []securitySession{}
	for key, expiry := range a.sessions {
		metadata, exists := a.sessionMeta[key]
		if !exists {
			// Production sessions always have metadata at creation. Do not invent
			// another client's IP/agent when reading a legacy in-memory entry.
			if a.sessionMeta == nil {
				a.sessionMeta = map[string]sessionMetadata{}
			}
			metadata = sessionMetadata{ID: randomToken(), CreatedAt: expiry.Add(-12 * time.Hour), IP: "unknown"}
			a.sessionMeta[key] = metadata
		}
		lastSeen := ""
		if !metadata.LastSeenAt.IsZero() {
			lastSeen = metadata.LastSeenAt.UTC().Format(time.RFC3339)
		}
		result = append(result, securitySession{ID: metadata.ID, Current: key == current,
			CreatedAt: metadata.CreatedAt.UTC().Format(time.RFC3339), LastSeenAt: lastSeen,
			ExpiresAt: expiry.UTC().Format(time.RFC3339), Client: metadata.Client, IP: metadata.IP})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Current != result[j].Current {
			return result[i].Current
		}
		if result[i].CreatedAt != result[j].CreatedAt {
			return result[i].CreatedAt > result[j].CreatedAt
		}
		return result[i].ID < result[j].ID
	})
	reply(w, 200, map[string]any{"sessions": result})
}

func (a *App) deleteSecuritySession(w http.ResponseWriter, r *http.Request) {
	a.authMu.Lock()
	defer a.authMu.Unlock()
	if !a.requireSecuritySessionLocked(w, r) {
		return
	}
	a.expireSecuritySessionsLocked()
	for key, metadata := range a.sessionMeta {
		if metadata.ID != r.PathValue("id") {
			continue
		}
		delete(a.sessions, key)
		delete(a.sessionMeta, key)
		current := key == sessionKey(r)
		if current {
			clearSessionCookie(w, r)
		}
		reply(w, 200, map[string]bool{"ok": true, "relogin": current})
		return
	}
	fail(w, 404, "会话不存在或已过期，请刷新列表")
}

func (a *App) revokeSecuritySessions(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Scope string `json:"scope"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Scope != "others" && in.Scope != "all" {
		fail(w, 400, "会话注销范围不正确")
		return
	}
	a.authMu.Lock()
	defer a.authMu.Unlock()
	if !a.requireSecuritySessionLocked(w, r) {
		return
	}
	a.expireSecuritySessionsLocked()
	current := sessionKey(r)
	for key := range a.sessions {
		if in.Scope == "all" || key != current {
			delete(a.sessions, key)
			delete(a.sessionMeta, key)
		}
	}
	if in.Scope == "all" {
		clearSessionCookie(w, r)
	}
	reply(w, 200, map[string]bool{"ok": true, "relogin": in.Scope == "all"})
}
