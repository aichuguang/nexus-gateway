package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"w103d/gateway/internal/model"
)

// RunningProxy owns only a child created by EnsureRunning. Reusing a verified
// external instance never grants permission to signal it or trust its PID file
// for shutdown. A non-owned handle has a nil Done channel.
type RunningProxy struct {
	process proxyProcess
	done    chan error
	exited  chan struct{}
	quit    sync.Once
	kill    sync.Once
	quitErr error
	killErr error
}

type proxyProcess interface {
	PID() int
	Wait() error
	Signal(os.Signal) error
	Kill() error
}

type nginxChild struct{ command *exec.Cmd }

func (p *nginxChild) PID() int                 { return p.command.Process.Pid }
func (p *nginxChild) Wait() error              { return p.command.Wait() }
func (p *nginxChild) Signal(s os.Signal) error { return p.command.Process.Signal(s) }
func (p *nginxChild) Kill() error              { return p.command.Process.Kill() }

func runningProxy(process proxyProcess) *RunningProxy {
	p := &RunningProxy{process: process, done: make(chan error, 1), exited: make(chan struct{})}
	go func() {
		err := process.Wait()
		p.done <- err
		close(p.done)
		close(p.exited)
	}()
	return p
}

func (p *RunningProxy) Owned() bool { return p != nil && p.process != nil }

// Done reports the owned child's exit once. It is nil for an existing proxy
// controlled by systemd or another owner; the application does not supervise it.
func (p *RunningProxy) Done() <-chan error {
	if p == nil {
		return nil
	}
	return p.done
}

// Stop gracefully stops this handle's child. If the caller's deadline expires,
// it kills that same os.Process (never a process selected from nginx.pid) and
// briefly waits for it to be reaped. Calling Stop repeatedly is safe.
func (p *RunningProxy) Stop(ctx context.Context) error {
	if !p.Owned() {
		return nil
	}
	select {
	case <-p.exited:
		return nil
	default:
	}
	p.quit.Do(func() { p.quitErr = p.process.Signal(proxyQuitSignal()) })
	if p.quitErr != nil && !errors.Is(p.quitErr, os.ErrProcessDone) {
		return errors.Join(p.quitErr, p.forceStop())
	}
	select {
	case <-p.exited:
		return nil
	case <-ctx.Done():
		return errors.Join(ctx.Err(), p.forceStop())
	}
}

func (p *RunningProxy) forceStop() error {
	select {
	case <-p.exited:
		return nil
	default:
	}
	p.kill.Do(func() { p.killErr = p.process.Kill() })
	if p.killErr != nil && !errors.Is(p.killErr, os.ErrProcessDone) {
		return fmt.Errorf("cannot stop the owned nginx child: %w", p.killErr)
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-p.exited:
		return nil
	case <-timer.C:
		return fmt.Errorf("owned nginx child has not exited after termination")
	}
}

// Hooks keep startup tests isolated from system users, ports and processes.
// They cannot be configured through flags, persisted state or the HTTP API.
type proxyStartupHooks struct {
	prepare       func() error
	validate      func(context.Context, ...string) error
	start         func(...string) (proxyProcess, error)
	inspect       func(int, string, string) (bool, error)
	portAvailable func(context.Context) error
	probe         func(context.Context, string) error
	waitTimeout   time.Duration
	pollInterval  time.Duration
	stopTimeout   time.Duration
}

func (e *Engine) proxyHooks() proxyStartupHooks {
	h := proxyStartupHooks{
		prepare: e.Prepare, validate: e.validateStartupConfig, start: e.startProxy,
		inspect: inspectNginxMaster, portAvailable: generationPortAvailable,
		probe: e.startupProbe, waitTimeout: 8 * time.Second,
		pollInterval: 100 * time.Millisecond, stopTimeout: 12 * time.Second,
	}
	if custom := e.startupHooks; custom != nil {
		if custom.prepare != nil {
			h.prepare = custom.prepare
		}
		if custom.validate != nil {
			h.validate = custom.validate
		}
		if custom.start != nil {
			h.start = custom.start
		}
		if custom.inspect != nil {
			h.inspect = custom.inspect
		}
		if custom.portAvailable != nil {
			h.portAvailable = custom.portAvailable
		}
		if custom.probe != nil {
			h.probe = custom.probe
		}
		if custom.waitTimeout > 0 {
			h.waitTimeout = custom.waitTimeout
		}
		if custom.pollInterval > 0 {
			h.pollInterval = custom.pollInterval
		}
		if custom.stopTimeout > 0 {
			h.stopTimeout = custom.stopTimeout
		}
	}
	return h
}

// EnsureRunning starts an isolated proxy from an already-applied snapshot, or
// an explicitly empty initial state. The caller must hold the runtime lock and
// must not pass a draft. This method never writes state/applied.json or calls a
// DNS provider. The caller is responsible for Stop when its HTTP server exits.
func (e *Engine) EnsureRunning(ctx context.Context, state model.State, publicAddress string) (handle *RunningProxy, resultErr error) {
	config, _, err := e.Render(state, publicAddress)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if e.DryRun {
		return &RunningProxy{}, nil
	}
	if runtime.GOOS != "linux" && e.startupHooks == nil {
		return nil, fmt.Errorf("automatic managed nginx startup requires Linux")
	}
	configPath, _, err := e.paths()
	if err != nil {
		return nil, err
	}
	hooks := e.proxyHooks()
	prefix := filepath.Clean(e.DataDir) + string(filepath.Separator)
	previous, existed, err := readManagedConfig(configPath)
	if err != nil {
		return nil, err
	}
	running, err := e.existingProxy(hooks, configPath)
	if err != nil {
		return nil, err
	}
	expected := generation(state, publicAddress)
	if running {
		if !existed || !bytes.Equal(previous, []byte(config)) {
			return nil, fmt.Errorf("existing managed nginx configuration differs from the applied snapshot; stop its owner and retry; no running configuration was changed")
		}
		if err = hooks.validate(ctx, "-p", prefix, "-c", configPath, "-t"); err != nil {
			return nil, err
		}
		if err = hooks.probe(ctx, expected); err != nil {
			return nil, fmt.Errorf("existing managed nginx does not serve the applied generation; it was not changed: %w", err)
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		return &RunningProxy{}, nil
	}
	if err = hooks.portAvailable(ctx); err != nil {
		return nil, err
	}
	if err = hooks.prepare(); err != nil {
		return nil, err
	}
	check, err := os.CreateTemp(filepath.Dir(configPath), "nginx-start-check-*.conf")
	if err != nil {
		return nil, fmt.Errorf("cannot create temporary managed nginx configuration: %w", err)
	}
	checkPath := check.Name()
	defer os.Remove(checkPath)
	_, writeErr := check.WriteString(config)
	closeErr := check.Close()
	if writeErr != nil || closeErr != nil {
		return nil, fmt.Errorf("cannot write temporary managed nginx configuration: %w", errors.Join(writeErr, closeErr))
	}
	if err = hooks.validate(ctx, "-p", prefix, "-c", checkPath, "-t"); err != nil {
		return nil, err
	}
	// Do not replace a configuration if another instance appeared during -t.
	if running, err = e.existingProxy(hooks, configPath); err != nil {
		return nil, err
	} else if running {
		return nil, fmt.Errorf("managed nginx started concurrently; retry after checking its owner")
	}
	if err = hooks.portAvailable(ctx); err != nil {
		return nil, err
	}
	current, currentExists, err := readManagedConfig(configPath)
	if err != nil || currentExists != existed || !bytes.Equal(current, previous) {
		return nil, fmt.Errorf("managed nginx configuration changed during startup; nothing was replaced")
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = atomicWrite(configPath, []byte(config)); err != nil {
		return nil, fmt.Errorf("cannot activate managed nginx configuration: %w", err)
	}
	var owned *RunningProxy
	committed := false
	defer func() {
		if committed {
			return
		}
		if owned != nil {
			stopCtx, cancel := context.WithTimeout(context.Background(), hooks.stopTimeout)
			stopErr := owned.Stop(stopCtx)
			cancel()
			resultErr = errors.Join(resultErr, stopErr)
			select {
			case <-owned.exited:
				e.removeOwnedPID(owned.process.PID())
			default:
				// A still-running child may reread its config. Do not replace it.
				resultErr = errors.Join(resultErr, fmt.Errorf("configuration retained because the owned nginx child has not exited"))
				return
			}
		}
		resultErr = errors.Join(resultErr, restoreStartupConfig(configPath, []byte(config), previous, existed))
	}()
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	child, err := hooks.start("-p", prefix, "-c", configPath, "-g", "daemon off;")
	if err != nil {
		return nil, fmt.Errorf("cannot start managed nginx: %w", err)
	}
	owned = runningProxy(child)
	if err = e.waitForProxy(ctx, hooks, owned, expected); err != nil {
		return nil, fmt.Errorf("managed nginx failed to become ready; inspect %s: %w", filepath.Join(e.DataDir, "error.log"), err)
	}
	committed = true
	return owned, nil
}

func (e *Engine) existingProxy(hooks proxyStartupHooks, configPath string) (bool, error) {
	pid, err := readProxyPID(filepath.Join(e.DataDir, "nginx.pid"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return hooks.inspect(pid, configPath, e.nginxBinary())
}

func readProxyPID(path string) (int, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || info.Size() > 32 {
		return 0, fmt.Errorf("managed nginx PID file is not a small regular file; inspect it before retrying")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		return 0, fmt.Errorf("managed nginx PID file is invalid; inspect it before retrying")
	}
	return pid, nil
}

func (e *Engine) removeOwnedPID(pid int) {
	path := filepath.Join(e.DataDir, "nginx.pid")
	if current, err := readProxyPID(path); err == nil && current == pid {
		_ = os.Remove(path)
	}
}

func readManagedConfig(path string) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("managed nginx configuration must be a regular file")
	}
	data, err := os.ReadFile(path)
	return data, true, err
}

func restoreStartupConfig(path string, written, previous []byte, existed bool) error {
	current, currentExists, err := readManagedConfig(path)
	if err != nil || !currentExists || !bytes.Equal(current, written) {
		return fmt.Errorf("managed configuration changed after startup began; rollback refused to overwrite it")
	}
	if existed {
		if err = atomicWrite(path, previous); err != nil {
			return fmt.Errorf("cannot restore the previous managed nginx configuration: %w", err)
		}
		return nil
	}
	if err = os.Remove(path); err != nil {
		return fmt.Errorf("cannot remove the failed initial nginx configuration: %w", err)
	}
	return nil
}

func (e *Engine) waitForProxy(ctx context.Context, hooks proxyStartupHooks, owned *RunningProxy, expected string) error {
	ctx, cancel := context.WithTimeout(ctx, hooks.waitTimeout)
	defer cancel()
	ticker := time.NewTicker(hooks.pollInterval)
	defer ticker.Stop()
	var lastErr error
	for {
		select {
		case err := <-owned.Done():
			return fmt.Errorf("nginx exited before becoming ready: %v", err)
		case <-ctx.Done():
			return errors.Join(ctx.Err(), lastErr)
		default:
		}
		pid, pidErr := readProxyPID(filepath.Join(e.DataDir, "nginx.pid"))
		if pidErr == nil && pid == owned.process.PID() {
			lastErr = hooks.probe(ctx, expected)
			if lastErr == nil {
				select {
				case err := <-owned.Done():
					return fmt.Errorf("nginx exited while confirming readiness: %v", err)
				case <-ctx.Done():
					return ctx.Err()
				default:
					return nil
				}
			}
		} else {
			lastErr = fmt.Errorf("waiting for the owned nginx child to create its PID file")
		}
		select {
		case err := <-owned.Done():
			return fmt.Errorf("nginx exited before becoming ready: %v", err)
		case <-ctx.Done():
			return errors.Join(ctx.Err(), lastErr)
		case <-ticker.C:
		}
	}
}

func (e *Engine) nginxBinary() string {
	if e.NginxBin != "" {
		return e.NginxBin
	}
	return "nginx"
}

func (e *Engine) startProxy(args ...string) (proxyProcess, error) {
	cmd := exec.Command(e.nginxBinary(), args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &nginxChild{command: cmd}, nil
}

func (e *Engine) validateStartupConfig(ctx context.Context, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.nginxBinary(), args...)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("nginx configuration check interrupted: %w", ctx.Err())
	}
	message := strings.TrimSpace(string(output))
	if len(message) > 2048 {
		message = message[len(message)-2048:]
	}
	return fmt.Errorf("nginx configuration check failed: %w: %s", err, message)
}

func generationPortAvailable(ctx context.Context) error {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp4", "127.0.0.1:18079")
	if err != nil {
		return fmt.Errorf("loopback proxy verification port 18079 is occupied or unavailable; no existing listener was changed: %w", err)
	}
	return listener.Close()
}

func (e *Engine) startupProbe(ctx context.Context, expected string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.generationProbe != nil {
		return e.generationProbe(expected)
	}
	return verifyGenerationContext(ctx, "http://127.0.0.1:18079/__gateway_generation", expected)
}

// Nginx rewrites argv into a master-process title. Match one exact -c argument,
// including its boundary, rather than authorizing a PID by a path substring.
func nginxMasterConfigMatches(cmdline []byte, configPath string) bool {
	title := strings.TrimRight(string(cmdline), "\x00")
	if !strings.HasPrefix(title, "nginx: master process ") || strings.ContainsRune(title, '\x00') || strings.Count(title, " -c ") != 1 {
		return false
	}
	_, argument, _ := strings.Cut(title, " -c ")
	if !strings.HasPrefix(argument, configPath) {
		return false
	}
	rest := argument[len(configPath):]
	return rest == "" || strings.HasPrefix(rest, " -")
}
