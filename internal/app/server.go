package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"w103d/gateway/internal/buildinfo"
	"w103d/gateway/internal/engine"
	"w103d/gateway/internal/model"
)

// proxyEngine keeps proxy activation independently testable from state storage.
type proxyEngine interface {
	Render(model.State, string) (string, []string, error)
	Apply(model.State, string) (string, error)
	VerifyTLS(model.State, string) error
	VerifyApplied(model.State, string) error
}

type App struct {
	runtimeWriter       func(string, any) error // Persistence fault injection in tests only.
	external            externalStore
	traffic             trafficStore
	history             historyStore
	monitor             monitorStore
	notifications       notificationStore
	lifecycle           appLifecycle
	changeMu            sync.Mutex
	jobs                publishJobStore
	mu                  sync.Mutex
	state               model.State
	opts                Options
	binaryPath          string
	engine              proxyEngine
	authMu              sync.Mutex
	sessions            map[string]time.Time
	sessionMeta         map[string]sessionMetadata
	attempts            map[string]loginAttempt
	passwordHash        string
	receiptsInitialized bool            // guarded by changeMu
	pendingCreates      map[string]bool // prepared local creates; guarded by changeMu
	static              fs.FS
	dnsMu               sync.Mutex
	dnsPending          map[string]dnsPreview
	dnsClient           dnsAPI
	convergence         convergenceStatus
	// zoneMemo caches which account and zone own which domain, so resolving a
	// name is not a cloud call per record. Guarded by mu.
	zoneMemo   []zoneEntry
	zoneMemoAt time.Time
	// edgeMu guards the EdgeOne side. It is separate from dnsMu because an
	// EdgeOne reconciliation makes several cloud calls over tens of seconds, and
	// holding the DNS lock for that long would stall a convergence pass that
	// needs to write the base station.
	edgeMu              sync.Mutex
	edgeClient          teoAPI
	sslClient           sslAPI
	cloudflareMu        sync.Mutex
	cloudflareClient    cloudflareAPI
	cloudflarePending   map[string]cloudflarePreview
	cloudflareResolve   func(context.Context, string) (cfDNSAnswer, error)
	cloudflareProbe     func(context.Context, string, string, string) error // test seam; never supplied by HTTP
	cloudflareTXTLookup func(context.Context, string) ([]string, error)     // DNS visibility test seam
	edgePending         map[string]edgePreview
	// edgeMemo caches the sites each account can reach. Guarded by mu.
	edgeMemo   []edgeSite
	edgeMemoAt time.Time
	// publicHost is a seam for tests to describe this machine's addresses without
	// touching the real network. It is nil in production.
	publicHost func(context.Context) ([]publicCandidate, string, []string)
}

// inspectHost reads the machine's public address state through the test seam
// when one is installed.
func (a *App) inspectHost(ctx context.Context) ([]publicCandidate, string, []string) {
	if a.publicHost != nil {
		return a.publicHost(ctx)
	}
	return inspectPublicHost(ctx)
}

func New(o Options, static fs.FS) (*App, error) {
	if e := ValidateOptions(o); e != nil {
		return nil, e
	}
	s, e := LoadState(o)
	if e != nil {
		return nil, e
	}
	o.CertdURL = s.Settings.CertdURL
	a := &App{state: s, opts: o, binaryPath: runningBinaryPath(), static: static, sessions: map[string]time.Time{}, attempts: map[string]loginAttempt{}, engine: &engine.Engine{DataDir: o.DataDir, NginxBin: o.Nginx, ConfigPath: filepath.Join(o.DataDir, "managed-nginx.conf"), CacheDir: o.CacheDir, DryRun: o.DryRun}}
	if e = a.initAuth(); e != nil {
		return nil, e
	}
	a.jobs.mu.Lock()
	a.loadPublishJobsLocked()
	a.jobs.mu.Unlock()
	a.recordHistoryLocked()
	return a, nil
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, msg string) {
	reply(w, status, map[string]string{"error": msg})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		fail(w, 415, "需要 JSON 请求")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		fail(w, 400, "请求格式不正确")
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		fail(w, 400, "请求中存在多余内容")
		return false
	}
	return true
}
func (a *App) saveLocked() error {
	if tx, err := readRuntimeTransaction(a.opts.DataDir); err != nil {
		return err
	} else if tx.Phase != "" && tx.Phase != "done" {
		return errApplyNeedsReview
	}
	if err := writeJSON(filepath.Join(a.opts.DataDir, "state.json"), a.state); err != nil {
		return err
	}
	a.recordHistoryLocked()
	return nil
}
func (a *App) stateResponse(w http.ResponseWriter) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.replyStateLocked(w)
}

// stateView is the draft as the wire sees it, plus the one thing a draft cannot
// say about itself: which parts of it are not running yet. Every handler that
// answers "here is the state" goes through here, so a draft and the account of
// what is still unpublished can never be sent apart.
type stateView struct {
	model.State
	// Runtime belongs to this process, not the saved draft or an exported backup.
	Runtime *runtimePaths `json:"runtime,omitempty"`
	// UnappliedServiceIDs names the services whose configuration differs from the
	// running snapshot. It is a list beside the services rather than a flag on
	// each one because the same service object is written straight back on save:
	// a field filled in at reply time would be persisted by the next write and
	// would then be a claim about a snapshot nobody compared it to.
	UnappliedServiceIDs []string `json:"unappliedServiceIds"`
	// UnappliedElsewhere is true when something with no row to hang a tag on also
	// differs: a deleted service, or a changed gateway address. It still has to
	// be applied, so the dashboard still has to say so.
	UnappliedElsewhere bool `json:"unappliedElsewhere"`
}

// replyStateLocked answers with the current state. Callers hold a.mu, the same
// way saveLocked's callers do.
func (a *App) replyStateLocked(w http.ResponseWriter) {
	reply(w, 200, a.stateViewLocked())
}

func (a *App) stateViewLocked() stateView {
	applied, err := a.publicAppliedState()
	ids, elsewhere := draftDelta(a.state, applied)
	if err != nil {
		// Nothing readable is running, so every service is unapplied -- the
		// operator has the most to do, and reporting the opposite would hide it.
		// But an absent snapshot cannot *disagree* with the gateway address:
		// there is no running address for it to differ from, and a banner saying
		// "you deleted a service or changed the address" would be a false alarm
		// on a gateway that has simply never been published. Every service
		// already carries its own tag in that case, so nothing is hidden.
		elsewhere = false
	}
	state := a.state
	state.Certificates = slices.Clone(a.state.Certificates)
	for i := range state.Certificates {
		// A restored draft can carry paths from the previous installation.
		// The control socket always reads the running data directory instead.
		state.Certificates[i].InboxPath = filepath.Join(a.opts.DataDir, "cert-inbox", state.Certificates[i].ID)
	}
	runtime := a.runtimePaths()
	return stateView{State: state, Runtime: &runtime, UnappliedServiceIDs: ids, UnappliedElsewhere: elsewhere}
}

// draftDelta compares the draft against the running snapshot service by service,
// using the generation hash's own definition of what a service is. A tag that lit
// up for a renamed service — which changes nothing the proxy serves — would teach
// the operator that tags mean nothing, and then a real one would be ignored too.
func draftDelta(draft, applied model.State) ([]string, bool) {
	return serviceDelta(draft, applied, true)
}

func serviceDelta(draft, applied model.State, includeEntryPolicy bool) ([]string, bool) {
	live := make(map[string]string, len(applied.Services))
	for _, s := range applied.Services {
		live[s.ID] = engine.ServiceGeneration(applied, s)
	}
	ids := []string{}
	inDraft := make(map[string]bool, len(draft.Services))
	for _, s := range draft.Services {
		inDraft[s.ID] = true
		previous, _ := findService(applied.Services, s.ID)
		if g, ok := live[s.ID]; !ok || g != engine.ServiceGeneration(draft, s) || includeEntryPolicy && !sameEntryPolicy(previous, s) {
			ids = append(ids, s.ID)
		}
	}
	elsewhere := draft.Settings.GatewayIP != applied.Settings.GatewayIP
	for _, s := range applied.Services {
		if !inDraft[s.ID] {
			elsewhere = true
		}
	}
	return ids, elsewhere
}

// servedUnapplied answers the only question a gate about "has this been applied?"
// should ask: would applying now change anything the proxy serves?
//
// It exists because a revision number answers a different, larger question. A
// revision moves for edits that change nothing the proxy serves — turning a
// service's entry mode to EdgeOne changes no server block, though it does change
// what that name's DNS record must say; adding a service and deleting it again
// changes nothing at all — and every gate that read the number alone refused
// those. That refusal was worse than it looked: the row tags and the banner are
// driven by this same field set, so they showed nothing worth applying, and the
// operator was told to apply a draft that every page said did not exist.
func servedUnapplied(draft, applied model.State) bool {
	pending, elsewhere := serviceDelta(draft, applied, false)
	return len(pending) > 0 || elsewhere
}

// appliedSnapshotLocked reads the applied snapshot and checks that it is the one
// the ledger names. A snapshot that cannot say which apply produced it is not
// evidence about what is running. Callers hold mu.
func (a *App) appliedSnapshotLocked() (model.State, error) {
	applied, err := a.publicAppliedState()
	if err != nil {
		return applied, err
	}
	if applied.Revision != applied.AppliedRevision || applied.AppliedRevision != a.state.AppliedRevision {
		return applied, errors.New("已应用快照与当前配置不一致，请重新应用配置")
	}
	return applied, nil
}

// Version identifies the build an operator is running. It sits next to the mode
// it is reported with so that the two places which print it cannot drift apart:
// a binary that names itself one version on the command line and another over
// HTTP is worse than one with no version at all.
var Version = buildinfo.Version

// register installs the whole route table on a bare mux. It is separate from
// Handler so tests can ask which API paths exist without going through the
// session, origin and CSRF guards.
func (a *App) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		info := buildinfo.Current()
		reply(w, 200, map[string]string{"status": "ok", "version": info.Version, "mode": "tunnel-diversion",
			"commit": info.Commit, "buildTime": info.BuildTime, "dirty": info.Dirty})
	})
	mux.HandleFunc("GET /api/session", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]bool{"authenticated": a.authenticated(r)})
	})
	mux.HandleFunc("POST /api/login", a.login)
	mux.HandleFunc("POST /api/logout", a.logout)
	mux.HandleFunc("GET /api/profile", a.getProfile)
	mux.HandleFunc("PUT /api/profile", a.changing(a.updateProfile))
	mux.HandleFunc("GET /api/history", a.listHistory)
	mux.HandleFunc("POST /api/history/{id}/preview", a.previewHistory)
	mux.HandleFunc("POST /api/history/{id}/restore", a.changing(a.restoreHistory))
	mux.HandleFunc("POST /api/backups/download", a.changing(a.downloadBackup))
	mux.HandleFunc("GET /api/security/sessions", a.listSecuritySessions)
	mux.HandleFunc("POST /api/security/password", a.changeSecurityPassword)
	mux.HandleFunc("DELETE /api/security/sessions/{id}", a.deleteSecuritySession)
	mux.HandleFunc("POST /api/security/sessions/revoke", a.revokeSecuritySessions)
	mux.HandleFunc("GET /api/monitor/settings", a.getMonitorSettings)
	mux.HandleFunc("PUT /api/monitor/settings", a.changing(a.updateMonitorSettings))
	mux.HandleFunc("GET /api/monitor/status", a.getMonitorStatus)
	mux.HandleFunc("GET /api/traffic", a.getTraffic)
	mux.HandleFunc("POST /api/monitor/check", a.checkMonitor)
	mux.HandleFunc("GET /api/notifications/channels", a.listNotificationChannels)
	mux.HandleFunc("POST /api/notifications/channels", a.changing(a.createNotificationChannel))
	mux.HandleFunc("PUT /api/notifications/channels/{id}", a.changing(a.updateNotificationChannel))
	mux.HandleFunc("DELETE /api/notifications/channels/{id}", a.changing(a.deleteNotificationChannel))
	mux.HandleFunc("POST /api/notifications/channels/{id}/test", a.testNotificationChannel)
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) { a.stateResponse(w) })
	mux.HandleFunc("POST /api/services", a.changing(a.idempotentCreate(a.createService)))
	mux.HandleFunc("PUT /api/services/{id}", a.changing(a.updateService))
	mux.HandleFunc("DELETE /api/services/{id}", a.changing(a.deleteService))
	mux.HandleFunc("POST /api/services/{id}/probe", a.probeService)
	mux.HandleFunc("POST /api/services/{id}/public-check", a.checkPublicService)
	mux.HandleFunc("POST /api/services/{id}/external-check", a.checkServiceExternal)
	mux.HandleFunc("GET /api/external-probes", a.getExternalProbes)
	mux.HandleFunc("GET /api/external-probes/limits", a.getExternalLimits)
	mux.HandleFunc("POST /api/external-probes", a.startExternalProbe)
	mux.HandleFunc("POST /api/services/{id}/publish", a.changing(a.publishService))
	mux.HandleFunc("POST /api/services/{id}/publish-jobs", a.createPublishJob)
	mux.HandleFunc("GET /api/publish-jobs", a.listPublishJobs)
	mux.HandleFunc("GET /api/operation-receipts/{requestId}", a.getOperationReceipt)
	mux.HandleFunc("GET /api/preview", a.preview)
	mux.HandleFunc("POST /api/apply", a.changing(a.apply))
	mux.HandleFunc("POST /api/certificates", a.changing(a.idempotentCreate(a.createCertificate)))
	mux.HandleFunc("POST /api/certificates/{id}/import", a.changing(a.importCertificate))
	mux.HandleFunc("DELETE /api/certificates/{id}", a.changing(a.deleteCertificate))
	mux.HandleFunc("GET /api/diagnostics", a.diagnostics)
	mux.HandleFunc("GET /api/public/preflight", a.publicPreflight)
	mux.HandleFunc("GET /api/public/address", a.publicAddressSummary)
	mux.HandleFunc("GET /api/dns/accounts", a.listDNSAccounts)
	mux.HandleFunc("GET /api/dns/bindings", a.handleDNSBindings)
	mux.HandleFunc("PUT /api/dns/bindings", a.changing(a.handleDNSBindings))
	mux.HandleFunc("PATCH /api/dns/accounts/{id}", a.changing(a.updateDNSAccount))
	mux.HandleFunc("POST /api/dns/accounts", a.changing(a.idempotentCreate(a.createDNSAccount)))
	mux.HandleFunc("DELETE /api/dns/accounts/{id}", a.changing(a.deleteDNSAccount))
	mux.HandleFunc("GET /api/dns/records", a.listDNSRecords)
	mux.HandleFunc("GET /api/dns/records/{id}/observation", a.observeDNSRecord)
	mux.HandleFunc("GET /api/dns/records/{id}/accounts", a.listDNSRecordAccounts)
	mux.HandleFunc("POST /api/dns/records/{id}/account", a.changing(a.selectDNSRecordAccount))
	mux.HandleFunc("POST /api/dns/records/{id}/preview", a.previewDNSRecord)
	mux.HandleFunc("POST /api/dns/records/{id}/apply", a.changing(a.applyDNSRecord))
	mux.HandleFunc("POST /api/dns/records/{id}/recover-overwrite", a.changing(a.recoverDNSReplacement))
	mux.HandleFunc("GET /api/dns/status", a.dnsStatus)
	mux.HandleFunc("GET /api/edgeone/accounts", a.listEdgeOneAccounts)
	mux.HandleFunc("GET /api/edge/accounts", a.listUnifiedEdgeAccounts)
	mux.HandleFunc("POST /api/edge/accounts", a.changing(a.idempotentCreate(a.createUnifiedEdgeAccount)))
	mux.HandleFunc("PATCH /api/edge/accounts/{provider}/{id}", a.changing(a.updateUnifiedEdgeAccount))
	mux.HandleFunc("DELETE /api/edge/accounts/{provider}/{id}", a.changing(a.deleteUnifiedEdgeAccount))
	mux.HandleFunc("GET /api/edge/hosts", a.unifiedEdgeHosts)
	mux.HandleFunc("GET /api/edge/hosts/{host}/preview", a.previewUnifiedEdgeHost)
	mux.HandleFunc("POST /api/edge/hosts/{host}/apply", a.changing(a.applyUnifiedEdgeHost))
	mux.HandleFunc("GET /api/cloudflare/accounts", a.listCloudflareAccounts)
	mux.HandleFunc("POST /api/cloudflare/accounts/discover", a.discoverCloudflareAccounts)
	mux.HandleFunc("POST /api/cloudflare/accounts", a.changing(a.idempotentCreate(a.createCloudflareAccount)))
	mux.HandleFunc("PATCH /api/cloudflare/accounts/{id}", a.changing(a.updateCloudflareAccount))
	mux.HandleFunc("DELETE /api/cloudflare/accounts/{id}", a.changing(a.deleteCloudflareAccount))
	mux.HandleFunc("GET /api/cloudflare/hosts", a.cloudflareHosts)
	mux.HandleFunc("POST /api/cloudflare/hosts/{host}/check", a.checkCloudflarePublishedEntry)
	mux.HandleFunc("POST /api/cloudflare/hosts/{host}/delegate", a.changing(a.delegateCloudflareDCV))
	mux.HandleFunc("POST /api/cloudflare/hosts/{host}/http-validation", a.changing(a.startCloudflareHTTPValidation))
	mux.HandleFunc("GET /api/cloudflare/preview", a.previewCloudflareHost)
	mux.HandleFunc("POST /api/cloudflare/apply", a.changing(a.applyCloudflareHost))
	mux.HandleFunc("POST /api/edgeone/accounts", a.changing(a.idempotentCreate(a.createEdgeOneAccount)))
	mux.HandleFunc("POST /api/edgeone/accounts/{id}", a.changing(a.updateEdgeOneAccount))
	mux.HandleFunc("DELETE /api/edgeone/accounts/{id}", a.changing(a.deleteEdgeOneAccount))
	mux.HandleFunc("GET /api/edgeone/status", a.edgeStatus)
	mux.HandleFunc("POST /api/edgeone/hosts/{host}/preview", a.previewEdgeHost)
	mux.HandleFunc("POST /api/edgeone/hosts/{host}/apply", a.changing(a.applyEdgeHost))
	mux.HandleFunc("PUT /api/settings", a.changing(a.updateSettings))
	mux.HandleFunc("GET /api/export", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="gateway-config.json"`)
		a.mu.Lock()
		defer a.mu.Unlock()
		view := a.stateViewLocked()
		view.Runtime = nil
		reply(w, 200, view)
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { fail(w, 404, "接口不存在") })
	mux.HandleFunc("/", a.assets)
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	a.register(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		if r.Host != a.opts.Listen {
			fail(w, 403, "不接受此管理入口 Host")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
			if r.Method != "GET" && r.Method != "HEAD" {
				scheme := "http"
				if r.TLS != nil {
					scheme = "https"
				}
				origin := r.Header.Get("Origin")
				if r.Header.Get("X-Gateway-Request") != "1" || (origin != "" && origin != scheme+"://"+r.Host) {
					fail(w, 403, "请求来源校验失败")
					return
				}
			}
			publicProfile := r.URL.Path == "/api/profile" && (r.Method == http.MethodGet || r.Method == http.MethodHead)
			if r.URL.Path != "/api/login" && r.URL.Path != "/api/session" && !publicProfile && !a.authenticated(r) {
				fail(w, 401, "请先登录管理台")
				return
			}
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				ctx, finish, accepted := a.beginWork()
				if !accepted {
					w.Header().Set("Retry-After", "5")
					fail(w, http.StatusServiceUnavailable, errAppStopping.Error())
					return
				}
				defer finish()
				requestCtx, cancel := context.WithCancel(r.Context())
				stop := context.AfterFunc(ctx, cancel)
				defer stop()
				defer cancel()
				r = r.WithContext(requestCtx)
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func (a *App) assets(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "HEAD" {
		http.Error(w, "method not allowed", 405)
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "." || name == "" {
		name = "index.html"
	}
	if a.static == nil {
		http.Error(w, "UI build unavailable", 503)
		return
	}
	b, e := fs.ReadFile(a.static, name)
	if e != nil {
		if strings.Contains(path.Base(name), ".") {
			http.NotFound(w, r)
			return
		}
		name = "index.html"
		b, e = fs.ReadFile(a.static, name)
	}
	if e != nil {
		http.NotFound(w, r)
		return
	}
	switch path.Ext(name) {
	case ".html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
	case ".js":
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case ".svg":
		w.Header().Set("Content-Type", "image/svg+xml")
	case ".woff2":
		w.Header().Set("Content-Type", "font/woff2")
	}
	if r.Method == "GET" {
		_, _ = w.Write(b)
	}
}

func readLimitedFile(p string) ([]byte, error) {
	f, e := os.Open(p)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() || st.Size() > 2<<20 {
		return nil, errors.New("certificate file must be a regular file below 2 MiB")
	}
	return io.ReadAll(io.LimitReader(f, 2<<20+1))
}
