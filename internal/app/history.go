package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"w103d/gateway/internal/dnspod"
	"w103d/gateway/internal/engine"
	"w103d/gateway/internal/model"
)

const historyLimit = 30

// Deliberately not model.State: history cannot restore credentials, certificate
// material, cloud ledgers, live application markers or old management passwords.
type historyConfig struct {
	Services   []model.Service `json:"services"`
	BaseDomain string          `json:"baseDomain"`
	BaseTTL    int             `json:"baseTtl"`
}
type historyEntry struct {
	ID           string        `json:"id"`
	CreatedAt    string        `json:"createdAt"`
	Revision     int64         `json:"revision"`
	Summary      string        `json:"summary"`
	ServiceCount int           `json:"serviceCount"`
	Config       historyConfig `json:"config"`
}
type historyLog struct {
	Format  int            `json:"format"`
	Entries []historyEntry `json:"entries"`
}
type historyPreview struct {
	ID, Token, Fingerprint string
	Expires                time.Time
}
type historyStore struct {
	loaded  bool
	err     error
	log     historyLog
	preview historyPreview
}
type historyChange struct {
	Path   string `json:"path"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

func editableHistory(s model.State) historyConfig {
	services := clone(s).Services
	if services == nil {
		services = []model.Service{}
	}
	return historyConfig{Services: services, BaseDomain: s.Settings.BaseDomain, BaseTTL: s.Settings.EffectiveBaseTTL()}
}

// All history access is serialized by App.mu, including the save hook.
func (a *App) loadHistoryLocked() {
	if a.history.loaded {
		return
	}
	a.history.loaded = true
	a.history.log = historyLog{Format: 1, Entries: []historyEntry{}}
	p := filepath.Join(a.opts.DataDir, "config-history.json")
	info, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	bad := func() {
		a.history.err = errors.New("配置历史无法读取或已损坏，原文件未覆盖；请检查数据目录")
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4<<20 {
		bad()
		return
	}
	data, err := os.ReadFile(p)
	if err != nil || len(data) > 4<<20 {
		bad()
		return
	}
	var history historyLog
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&history) != nil || decoder.Decode(new(any)) != io.EOF || history.Format != 1 || len(history.Entries) > historyLimit {
		bad()
		return
	}
	seen := map[string]bool{}
	for _, entry := range history.Entries {
		if entry.ID == "" || seen[entry.ID] || entry.Revision < 1 || len(entry.Config.Services) > maxServices {
			bad()
			return
		}
		seen[entry.ID] = true
	}
	a.history.log = history
}

func (a *App) recordHistoryLocked() {
	a.loadHistoryLocked()
	if a.history.err != nil {
		return
	}
	config := editableHistory(a.state)
	if len(a.history.log.Entries) > 0 && reflect.DeepEqual(a.history.log.Entries[0].Config, config) {
		return
	}
	entry := historyEntry{ID: NewID(), CreatedAt: time.Now().UTC().Format(time.RFC3339), Revision: a.state.Revision,
		Summary: fmt.Sprintf("配置快照 · %d 个服务", len(config.Services)), ServiceCount: len(config.Services), Config: config}
	next := historyLog{Format: 1, Entries: append([]historyEntry{entry}, a.history.log.Entries...)}
	if len(next.Entries) > historyLimit {
		next.Entries = next.Entries[:historyLimit]
	}
	encoded, err := json.Marshal(next)
	if err == nil && len(encoded) > 4<<20 {
		err = errors.New("历史记录超过容量上限")
	}
	if err == nil {
		err = AtomicWrite(filepath.Join(a.opts.DataDir, "config-history.json"), encoded)
	}
	if err != nil {
		// The configuration is already committed. Do not report its save as a
		// failure and roll back only memory because a secondary history write failed.
		a.history.err = errors.New("配置已保存，但历史记录写入失败；请检查数据目录")
		log.Print(a.history.err)
		return
	}
	a.history.log = next
}

func (a *App) listHistory(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.loadHistoryLocked()
	entries := []map[string]any{}
	for _, entry := range a.history.log.Entries {
		entries = append(entries, map[string]any{"id": entry.ID, "createdAt": entry.CreatedAt, "revision": entry.Revision,
			"summary": entry.Summary, "serviceCount": entry.ServiceCount})
	}
	message := ""
	if a.history.err != nil {
		message = a.history.err.Error()
	}
	reply(w, 200, map[string]any{"entries": entries, "error": message})
}

func (a *App) historyEntryLocked(id string) (historyEntry, error) {
	a.loadHistoryLocked()
	if a.history.err != nil {
		return historyEntry{}, a.history.err
	}
	for _, entry := range a.history.log.Entries {
		if entry.ID == id {
			return entry, nil
		}
	}
	return historyEntry{}, errors.New("历史版本不存在或已超出保留数量")
}

func historyChanges(before, after historyConfig) []historyChange {
	changes := []historyChange{}
	if before.BaseDomain != after.BaseDomain {
		changes = append(changes, historyChange{"基站域名", before.BaseDomain, after.BaseDomain})
	}
	if before.BaseTTL != after.BaseTTL {
		changes = append(changes, historyChange{"基站 TTL", before.BaseTTL, after.BaseTTL})
	}
	old, next := map[string]model.Service{}, map[string]model.Service{}
	ids := map[string]bool{}
	for _, s := range before.Services {
		old[s.ID], ids[s.ID] = s, true
	}
	for _, s := range after.Services {
		next[s.ID], ids[s.ID] = s, true
	}
	order := []string{}
	for id := range ids {
		order = append(order, id)
	}
	sort.Strings(order)
	for _, id := range order {
		b, bok := old[id]
		n, nok := next[id]
		if bok && nok && reflect.DeepEqual(b, n) {
			continue
		}
		if bok && nok {
			fields := []struct {
				name          string
				before, after any
			}{
				{"名称", b.Name, n.Name}, {"图标", b.Icon, n.Icon},
				{"上游", b.Upstream, n.Upstream}, {"端口", b.Port, n.Port},
				{"域名", b.Domains, n.Domains}, {"证书", b.CertificateID, n.CertificateID},
				{"入口方式", b.EntryMode, n.EntryMode}, {"启用", b.Enabled, n.Enabled},
				{"边缘提供商", b.EffectiveEdgeProvider(), n.EffectiveEdgeProvider()}, {"边缘账号", b.EdgeAccountID, n.EdgeAccountID},
				{"使用优选", b.UsePreferred, n.UsePreferred}, {"优选域名", b.PreferredDomain, n.PreferredDomain},
			}
			for _, field := range fields {
				if !reflect.DeepEqual(field.before, field.after) {
					changes = append(changes, historyChange{"服务/" + id + "/" + field.name, field.before, field.after})
				}
			}
			continue
		}
		var bv, nv any
		if bok {
			bv = b
		}
		if nok {
			nv = n
		}
		changes = append(changes, historyChange{"服务/" + id, bv, nv})
	}
	return changes
}

func (a *App) historyCandidateLocked(entry historyEntry) (model.State, error) {
	config := entry.Config
	base := strings.ToLower(strings.TrimSpace(config.BaseDomain))
	if base != "" && (!engine.ValidDomain(base) || !strings.Contains(base, ".")) {
		return model.State{}, errors.New("历史基站域名无效")
	}
	if !dnspod.TTLAllowed(config.BaseTTL) {
		return model.State{}, errors.New("历史基站 TTL 无效")
	}
	if refusal := a.baseStationChangeRefusalLocked(base); refusal != "" {
		return model.State{}, errors.New(refusal)
	}
	next := clone(a.state)
	next.Services = append([]model.Service{}, config.Services...)
	next.Settings.BaseDomain, next.Settings.BaseTTL, next.Settings.AutoResolve = base, config.BaseTTL, false
	next.Revision++
	ids := map[string]model.Service{}
	for _, service := range next.Services {
		if _, exists := ids[service.ID]; exists || service.ID == "" {
			return model.State{}, errors.New("历史服务标识重复或缺失")
		}
		ids[service.ID] = service
		if err := edgeServiceAccountAvailable(next, service); err != nil {
			return model.State{}, err
		}
		if service.CertificateID != "" && a.certIndex(service.CertificateID) < 0 {
			return model.State{}, fmt.Errorf("服务「%s」引用的证书已不存在", service.Name)
		}
		if err := engine.ValidateService(service, next.Settings); err != nil {
			return model.State{}, err
		}
		for _, domain := range service.Domains {
			if strings.EqualFold(domain, base) {
				return model.State{}, errors.New("基站域名与服务域名冲突")
			}
		}
	}
	if err := a.assertHistoryDeletionSafe(ids); err != nil {
		return model.State{}, err
	}
	for _, before := range a.state.Services {
		if err := a.assertNamesWithdrawn(before, ids[before.ID]); err != nil {
			return model.State{}, err
		}
	}
	if _, _, err := a.engine.Render(next, a.publicAddressForRender()); err != nil {
		return model.State{}, err
	}
	return next, nil
}

// History changes only drafts, but must not erase a deletion's frozen identity
// or restore a retired one. This is checked again on commit: a deletion journal
// can change without changing the draft revision used by the preview token.
func (a *App) assertHistoryDeletionSafe(next map[string]model.Service) error {
	deletions, err := a.readServiceDeletions()
	if err != nil {
		return err
	}
	for id, service := range next {
		deletion := deletions.Entries[id]
		if deletion == nil {
			continue
		}
		index := a.serviceIndex(id)
		if deletion.Completed || index < 0 {
			return fmt.Errorf("服务「%s」已有删除保护记录，不能从历史恢复旧服务；如需使用，请重新创建服务", service.Name)
		}
		if !reflect.DeepEqual(a.state.Services[index], service) {
			return fmt.Errorf("服务「%s」正在删除，历史恢复不能修改其配置；请先保持原选项完成删除", service.Name)
		}
	}
	for _, service := range a.state.Services {
		deletion := deletions.Entries[service.ID]
		if deletion == nil || deletion.Completed {
			continue
		}
		if _, kept := next[service.ID]; !kept {
			return fmt.Errorf("服务「%s」正在删除，历史恢复不能移除该服务；请先保持原选项完成删除", service.Name)
		}
	}
	return nil
}

func (a *App) previewHistory(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Revision int64 `json:"revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if in.Revision != a.state.Revision {
		fail(w, 409, "配置已变化，请刷新后重新预览")
		return
	}
	entry, err := a.historyEntryLocked(r.PathValue("id"))
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	changes := historyChanges(editableHistory(a.state), entry.Config)
	warnings := []string{"仅恢复为草稿，不发布；自动解析将暂停，请核对后发布并按需重新开启。"}
	_, err = a.historyCandidateLocked(entry)
	canRestore := err == nil && len(changes) > 0
	if err != nil {
		warnings = append(warnings, err.Error())
	}
	token := ""
	if canRestore {
		token = randomToken()
		a.history.preview = historyPreview{ID: entry.ID, Token: token, Fingerprint: publishFingerprint(a.state), Expires: time.Now().Add(5 * time.Minute)}
	}
	reply(w, 200, map[string]any{"id": entry.ID, "revision": a.state.Revision, "token": token, "changes": changes, "warnings": warnings, "canRestore": canRestore})
}

func (a *App) restoreHistory(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Revision int64  `json:"revision"`
		Token    string `json:"token"`
		Confirm  bool   `json:"confirm"`
		Password string `json:"password"`
	}
	if !decode(w, r, &in) || !a.requirePassword(w, r, in.Password) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	preview := a.history.preview
	if !in.Confirm || in.Token == "" || in.Token != preview.Token || preview.ID != r.PathValue("id") || time.Now().After(preview.Expires) || in.Revision != a.state.Revision || preview.Fingerprint != publishFingerprint(a.state) {
		fail(w, 409, "恢复预览已失效，请重新预览并确认")
		return
	}
	a.history.preview = historyPreview{}
	entry, err := a.historyEntryLocked(preview.ID)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	next, err := a.historyCandidateLocked(entry)
	if err != nil {
		fail(w, 409, err.Error())
		return
	}
	before := a.state
	a.state = next
	if err := a.saveLocked(); err != nil {
		a.state = before
		fail(w, 500, "无法保存恢复草稿")
		return
	}
	a.dnsPending, a.edgePending = nil, nil
	reply(w, 200, map[string]any{"state": a.stateViewLocked(), "message": "已恢复为新草稿，尚未发布；自动解析已暂停。"})
}
