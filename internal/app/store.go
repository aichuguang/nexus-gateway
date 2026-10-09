package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"w103d/gateway/internal/atomicfile"
	"w103d/gateway/internal/engine"
	"w103d/gateway/internal/model"
)

type Options struct {
	Listen, GatewayIP, CertdURL, DataDir, Nginx, CacheDir string
	// PublicAddress pins the address services are published on. Empty means
	// "detect it", which is what a real deployment does. It exists for the
	// development dry-run, where there is no W103D to detect anything on.
	PublicAddress string
	DryRun        bool
	// Used only before any administrator hash or legacy initial-password exists.
	InitialKey        string
	RequireInitialKey bool
}

func NewID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func AtomicWrite(path string, data []byte) error {
	return atomicfile.WriteCompatible(path, data, atomicfile.Options{CreateParents: true})
}

func writeJSON(path string, value any) error {
	b, e := json.MarshalIndent(value, "", "  ")
	if e != nil {
		return e
	}
	if (filepath.Base(path) == "state.json" || filepath.Base(path) == "applied.json") && len(b)+1 > 2<<20 {
		return errors.New("state snapshot exceeds the 2 MiB readable limit; previous file was preserved")
	}
	return AtomicWrite(path, append(b, '\n'))
}

// Bootstrap is immutable and deliberately empty: an absent applied snapshot must
// never publish a saved draft, and a new installation must not invent services,
// certificates or credentials that the operator never created.
func initialState(o Options) model.State {
	return model.State{
		SchemaVersion:   model.SchemaVersion,
		Revision:        1,
		Settings:        model.Settings{GatewayIP: o.GatewayIP, CertdURL: o.CertdURL, AutoResolve: true},
		Services:        []model.Service{},
		Certificates:    []model.Certificate{},
		DNSAccounts:     []model.DNSAccount{},
		EdgeOneAccounts: []model.EdgeOneAccount{},
		EdgeOrigins:     []model.EdgeOrigin{},
	}
}

func ensureDataMode(o Options) error {
	marker := filepath.Join(o.DataDir, ".dry-run")
	_, err := os.Lstat(marker)
	if err == nil {
		if !o.DryRun {
			return errors.New("development data directory cannot be used for a live deployment")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !o.DryRun {
		return nil
	}
	for _, name := range []string{"state.json", "applied.json"} {
		if _, err := os.Lstat(filepath.Join(o.DataDir, name)); !errors.Is(err, os.ErrNotExist) {
			return errors.New("dry-run requires a separate new data directory; existing live state cannot be simulated in place")
		}
	}
	return AtomicWrite(marker, []byte("development-only; never deploy this directory\n"))
}

// checkSchema refuses an unrecognised schema instead of resetting it. An empty,
// truncated or future state file must never look like a fresh installation.
func checkSchema(s model.State) error {
	if s.SchemaVersion == 0 {
		return errors.New("state file has no schema version; refusing to guess or reset it")
	}
	if s.SchemaVersion != model.SchemaVersion {
		return fmt.Errorf("state file uses schema version %d but this build requires %d; refusing implicit migration", s.SchemaVersion, model.SchemaVersion)
	}
	return nil
}

func LoadState(o Options) (model.State, error) {
	var st model.State
	if !filepath.IsAbs(o.DataDir) {
		return st, errors.New("data directory must be absolute")
	}
	if err := os.MkdirAll(o.DataDir, 0700); err != nil {
		return st, err
	}
	if err := os.Chmod(o.DataDir, 0700); err != nil {
		return st, err
	}
	if err := ensureDataMode(o); err != nil {
		return st, err
	}
	data, err := os.ReadFile(filepath.Join(o.DataDir, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		st = initialState(o)
		return st, writeJSON(filepath.Join(o.DataDir, "state.json"), st)
	}
	if err != nil {
		return st, err
	}
	if len(data) > 2<<20 {
		return st, errors.New("state file too large")
	}
	if err = json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("state file invalid; refusing to reset: %w", err)
	}
	if st.SchemaVersion != model.SchemaVersion {
		return st, fmt.Errorf("state schema %d requires explicit offline state-migrate before starting schema %d", st.SchemaVersion, model.SchemaVersion)
	}
	if err = model.UpgradeSchema(&st); err != nil {
		return st, err
	}
	if err = checkSchema(st); err != nil {
		return st, err
	}
	if err = validateCertdURL(st.Settings.CertdURL); err != nil {
		return st, err
	}
	// An omitted optional endpoint must not invalidate an existing installation.
	if st.Settings.GatewayIP != o.GatewayIP || (o.CertdURL != "" && st.Settings.CertdURL != o.CertdURL) {
		return st, errors.New("startup settings differ from saved settings; refusing implicit migration")
	}
	return st, nil
}

func AppliedState(o Options) (model.State, error) {
	b, err := os.ReadFile(filepath.Join(o.DataDir, "applied.json"))
	if errors.Is(err, os.ErrNotExist) {
		return initialState(o), nil
	}
	var s model.State
	if err != nil {
		return s, err
	}
	if err = json.Unmarshal(b, &s); err != nil {
		return s, err
	}
	if s.SchemaVersion != model.SchemaVersion {
		return s, fmt.Errorf("applied schema %d requires explicit offline state-migrate", s.SchemaVersion)
	}
	if err = model.UpgradeSchema(&s); err != nil {
		return s, err
	}
	if err = checkSchema(s); err != nil {
		return s, err
	}
	return s, nil
}

func ValidateOptions(o Options) error {
	if !engine.ValidVPNIPv4(o.GatewayIP) {
		return errors.New("--gateway-ip is required and must be a valid unicast VPN IPv4 address")
	}
	host, _, err := net.SplitHostPort(o.Listen)
	if err != nil {
		return err
	}
	if host != o.GatewayIP && host != "127.0.0.1" {
		return errors.New("management listener must use the configured gateway IP or 127.0.0.1, never wildcard")
	}
	if !filepath.IsAbs(o.DataDir) || !filepath.IsAbs(o.CacheDir) {
		return errors.New("data and cache paths must be absolute")
	}
	return validateCertdURL(o.CertdURL)
}

func validateCertdURL(raw string) error {
	if raw == "" {
		return nil
	}
	invalid := errors.New("certd endpoint must be an HTTP(S) URL with a valid host and port, without userinfo or fragment")
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || strings.Contains(raw, "#") {
		return invalid
	}
	host := u.Hostname()
	if host == "" || strings.HasSuffix(u.Host, ":") {
		return invalid
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return invalid
		}
	}
	if ip := net.ParseIP(host); ip != nil {
		if strings.Contains(host, ":") != strings.HasPrefix(u.Host, "[") {
			return invalid
		}
		return nil
	}
	if strings.ContainsAny(u.Host, "[]") || len(host) > 253 {
		return invalid
	}
	for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return invalid
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return invalid
			}
		}
	}
	return nil
}

// Resource limits. They are deliberately modest: this gateway manages a private
// deployment, not a hosting platform.
//
// Every enabled service now renders its own server block, so the service ceiling
// is a real ceiling on rendered configuration rather than an abstract list
// length. It still has room for a machine exposing a hundred ports.
const (
	maxServices        = 200
	maxCertificates    = 50
	maxDNSAccounts     = 20
	maxEdgeOneAccounts = 10
)
