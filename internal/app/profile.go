package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Profile is display-only metadata, deliberately outside the publishable state.
type Profile struct {
	ProjectName string `json:"projectName"`
	ClientName  string `json:"clientName"`
	Logo        string `json:"logo,omitempty"`
}

func defaultProfile() Profile {
	return Profile{ProjectName: "澄光网关", ClientName: "本地网关"}
}

func normalizeProfile(p Profile) (Profile, error) {
	for _, name := range []*string{&p.ProjectName, &p.ClientName} {
		if !utf8.ValidString(*name) {
			return Profile{}, errors.New("名称必须是有效的 UTF-8 文本")
		}
		for _, r := range *name {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
				return Profile{}, errors.New("名称不能包含控制字符")
			}
		}
		*name = strings.TrimSpace(*name)
		if n := utf8.RuneCountInString(*name); n < 1 || n > 60 {
			return Profile{}, errors.New("名称须为 1 至 60 个字符")
		}
	}
	if err := validateProfileLogo(p.Logo); err != nil {
		return Profile{}, err
	}
	return p, nil
}

func (a *App) readProfile() (Profile, error) {
	b, err := readBackupFile(a.opts.DataDir, "profile.json")
	if errors.Is(err, os.ErrNotExist) {
		return defaultProfile(), nil
	}
	if err != nil {
		return Profile{}, err
	}
	var p Profile
	if err = json.Unmarshal(b, &p); err != nil {
		return Profile{}, err
	}
	return normalizeProfile(p)
}

func (a *App) getProfile(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p, err := a.readProfile()
	if err != nil {
		fail(w, 500, "无法读取配置资料，请检查资料文件")
		return
	}
	reply(w, 200, p)
}

func (a *App) updateProfile(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ProjectName string  `json:"projectName"`
		ClientName  string  `json:"clientName"`
		Logo        *string `json:"logo"`
	}
	if !decode(w, r, &in) {
		return
	}
	p := Profile{ProjectName: in.ProjectName, ClientName: in.ClientName}
	if in.Logo != nil {
		p.Logo = *in.Logo
	}
	p, err := normalizeProfile(p)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	// Older clients only send names. An explicit empty logo resets the mark;
	// omitting it must not remove an image saved by a newer client.
	if in.Logo == nil {
		current, err := a.readProfile()
		if err != nil {
			fail(w, 500, "\u65e0\u6cd5\u8bfb\u53d6\u914d\u7f6e\u8d44\u6599\uff0c\u8bf7\u68c0\u67e5\u8d44\u6599\u6587\u4ef6")
			return
		}
		p.Logo = current.Logo
	}
	if err := writeJSON(filepath.Join(a.opts.DataDir, "profile.json"), p); err != nil {
		fail(w, 500, "无法保存配置资料，请检查存储权限")
		return
	}
	reply(w, 200, p)
}
