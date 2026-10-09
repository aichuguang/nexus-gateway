package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"w103d/gateway/internal/app"
	"w103d/gateway/internal/buildinfo"
	"w103d/gateway/internal/engine"
	"w103d/gateway/internal/statecheck"
)

//go:embed all:web/dist
var assets embed.FS

const (
	defaultDataDir  = "/var/lib/gateway"
	defaultCacheDir = "/var/cache/gateway"
)

func options(f *flag.FlagSet) *app.Options {
	o := &app.Options{}
	f.String("config", "", "Startup JSON file (default: config.json beside the executable or in the working directory)")
	f.StringVar(&o.Listen, "listen", "127.0.0.1:18080", "Management listener (configured gateway IP or 127.0.0.1)")
	f.StringVar(&o.GatewayIP, "gateway-ip", "", "VPN IPv4 override; without --config this selects the legacy CLI configuration mode")
	f.StringVar(&o.CertdURL, "certd-url", "", "Optional certd HTTP(S) URL (empty preserves saved settings)")
	f.StringVar(&o.DataDir, "data-dir", defaultDataDir, "Persistent data directory")
	f.StringVar(&o.Nginx, "nginx", "/usr/sbin/nginx", "Nginx executable")
	f.StringVar(&o.CacheDir, "cache-dir", defaultCacheDir, "Nginx worker temporary directory")
	f.StringVar(&o.PublicAddress, "public-address", "", "Pin the published public address (development only; empty means detect)")
	f.BoolVar(&o.DryRun, "dry-run", false, "Development only; do not validate/reload Nginx")
	return o
}

func parseOptions(f *flag.FlagSet, args []string) (*app.Options, error) {
	o := options(f)
	if err := f.Parse(args); err != nil {
		return nil, err
	}
	if filepath.ToSlash(filepath.Clean(o.DataDir)) == "/var/lib/w103d-gateway" {
		explicitCache := false
		f.Visit(func(option *flag.Flag) {
			if option.Name == "cache-dir" {
				explicitCache = true
			}
		})
		if !explicitCache {
			// Existing systemd units allow writes only in their original cache.
			// Preserve that location when upgrading just the executable.
			o.CacheDir = "/var/cache/w103d-gateway"
		}
	}
	return o, nil
}

func run() error {
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && (!strings.HasPrefix(args[0], "-") || args[0] == "--version") {
		cmd = args[0]
		args = args[1:]
	}
	if cmd == "version" || cmd == "--version" {
		if len(args) == 1 && args[0] == "--json" {
			return json.NewEncoder(os.Stdout).Encode(buildinfo.Current())
		}
		if len(args) != 0 {
			return fmt.Errorf("usage: version [--json]")
		}
		fmt.Println("gateway " + app.Version)
		return nil
	}
	if cmd == "certificate-apply" {
		if len(args) == 0 {
			return fmt.Errorf("usage: certificate-apply ID [--data-dir PATH]")
		}
		id := args[0]
		f := flag.NewFlagSet(cmd, flag.ContinueOnError)
		dir := f.String("data-dir", defaultDataDir, "Persistent data directory")
		if e := f.Parse(args[1:]); e != nil {
			return e
		}
		return app.ActivateViaSocket(*dir, id)
	}
	if cmd == "state-check" || cmd == "state-migrate" {
		f := flag.NewFlagSet(cmd, flag.ContinueOnError)
		dir := f.String("data-dir", defaultDataDir, "Existing data directory; read-only offline preflight")
		allowLegacy := f.Bool("allow-legacy", false, "Read-only validation of known schema 4/5 predecessors")
		if err := f.Parse(args); err != nil {
			return err
		}
		if f.NArg() != 0 || !filepath.IsAbs(*dir) {
			return fmt.Errorf("usage: state-check --data-dir ABSOLUTE_PATH")
		}
		if cmd == "state-migrate" {
			if err := app.MigrateState(*dir); err != nil {
				return err
			}
		}
		report, err := statecheck.Inspect(*dir, *allowLegacy)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(report)
	}
	f := flag.NewFlagSet(cmd, flag.ContinueOnError)
	o, e := parseOptions(f, args)
	if e != nil {
		return e
	}
	executable, e := os.Executable()
	if e != nil {
		return e
	}
	workingDirectory, e := os.Getwd()
	if e != nil {
		return e
	}
	if e := applyStartupConfig(o, f, executable, workingDirectory); e != nil {
		return e
	}
	if e := app.ValidateOptions(*o); e != nil {
		return e
	}
	if cmd == "serve" && preparesRuntime(*o) {
		if e := preflightRuntime(*o); e != nil {
			return e
		}
	}
	if cmd == "render" || cmd == "serve" {
		unlock, err := app.AcquireRuntimeLock(o.DataDir)
		if err != nil {
			return err
		}
		defer unlock()
		if err = app.RecoverRuntimeState(o.DataDir); err != nil {
			return err
		}
	}
	switch cmd {
	case "render":
		if _, e := app.LoadState(*o); e != nil {
			return e
		}
		st, e := app.AppliedState(*o)
		if e != nil {
			return e
		}
		eng := engine.Engine{DataDir: o.DataDir, NginxBin: o.Nginx, ConfigPath: filepath.Join(o.DataDir, "managed-nginx.conf"), CacheDir: o.CacheDir, DryRun: o.DryRun}
		if e = eng.Prepare(); e != nil {
			return e
		}
		text, _, e := eng.Render(st, st.AppliedPublicAddress)
		if e != nil {
			return e
		}
		return app.AtomicWrite(eng.ConfigPath, []byte(text))
	case "serve":
		static, e := fs.Sub(assets, "web/dist")
		if e != nil {
			return e
		}
		server, e := app.New(*o, static)
		if e != nil {
			return e
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return serveWithRuntime(ctx, *o, server, os.Stdout, liveRuntimeOperations())
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}
func main() {
	if e := run(); e != nil {
		log.Fatal(e)
	}
}
