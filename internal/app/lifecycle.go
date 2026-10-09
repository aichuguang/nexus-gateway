package app

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

// Leave headroom for systemd's 20-second stop timeout. All shutdown work shares
// this budget; a stuck engine call must not turn cancellation into an endless wait.
const shutdownTimeout = 12 * time.Second

var errAppStopping = errors.New("管理程序正在停止，请稍后重新连接并核对操作结果")

type appLifecycle struct {
	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
	stopping bool
	workers  sync.WaitGroup
}

func (a *App) lifecycleContext() context.Context {
	a.lifecycle.mu.Lock()
	defer a.lifecycle.mu.Unlock()
	return a.lifecycleContextLocked()
}

func (a *App) lifecycleContextLocked() context.Context {
	if a.lifecycle.ctx == nil {
		a.lifecycle.ctx, a.lifecycle.cancel = context.WithCancel(context.Background())
	}
	return a.lifecycle.ctx
}

// Admission and the transition to stopping share a lock, so Wait can never race
// a new Add. This also works for handlers used without ServeContext in tests.
func (a *App) beginWork() (context.Context, func(), bool) {
	a.lifecycle.mu.Lock()
	defer a.lifecycle.mu.Unlock()
	ctx := a.lifecycleContextLocked()
	if a.lifecycle.stopping {
		return ctx, func() {}, false
	}
	a.lifecycle.workers.Add(1)
	return ctx, a.lifecycle.workers.Done, true
}

func (a *App) beginShutdown() <-chan struct{} {
	a.lifecycle.mu.Lock()
	a.lifecycleContextLocked()
	a.lifecycle.stopping = true
	a.lifecycle.cancel()
	a.lifecycle.mu.Unlock()
	done := make(chan struct{})
	go func() {
		a.lifecycle.workers.Wait()
		close(done)
	}()
	return done
}

func (a *App) Serve() error { return a.ServeContext(context.Background()) }

// ServeContext stops accepting writes, cancels cloud work and drains both HTTP
// listeners within one bounded interval. An App is single-use after shutdown.
func (a *App) ServeContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return nil
	}
	if a.lifecycleContext().Err() != nil {
		return errAppStopping
	}
	stopControl, err := a.startControl()
	if err != nil {
		return err
	}
	if !a.opts.DryRun {
		a.startConvergence()
		a.startPublishContinuation()
	}
	a.startMonitor()
	a.startTraffic()
	server := &http.Server{
		Addr: a.opts.Listen, Handler: a.Handler(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 45 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10,
		BaseContext: func(net.Listener) context.Context { return a.lifecycleContext() },
	}
	result := make(chan error, 1)
	go func() { result <- server.ListenAndServe() }()
	log.Printf("gateway private management listening on %s (public/DNS changes require explicit application)", a.opts.Listen)
	select {
	case <-ctx.Done():
	case err = <-result:
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	a.shutdown(stopCtx, server, stopControl)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (a *App) shutdown(ctx context.Context, server *http.Server, stopControl func(context.Context) error) {
	workers := a.beginShutdown()
	drainCtx := ctx
	if deadline, ok := ctx.Deadline(); ok {
		reserve := time.Second
		if remaining := time.Until(deadline); remaining < 2*reserve {
			reserve = remaining / 2
		}
		var cancel context.CancelFunc
		drainCtx, cancel = context.WithDeadline(ctx, deadline.Add(-reserve))
		defer cancel()
	}
	closed := make(chan struct{})
	go func() {
		var servers sync.WaitGroup
		servers.Add(2)
		go func() {
			defer servers.Done()
			if err := server.Shutdown(drainCtx); err != nil {
				_ = server.Close()
			}
		}()
		go func() { defer servers.Done(); _ = stopControl(drainCtx) }()
		servers.Wait()
		close(closed)
	}()
	select {
	case <-workers:
	case <-drainCtx.Done():
		log.Printf("shutdown deadline reached; unfinished writes require verification")
	}
	select {
	case <-closed:
	case <-drainCtx.Done():
		_ = server.Close()
	}
	// The job lock is separate from the proxy/cloud locks. Never wait on a.mu
	// here: a legacy engine operation may still own it until process exit.
	saved := make(chan struct{})
	go func() { defer close(saved); a.interruptPublishJobs() }()
	select {
	case <-saved:
	case <-ctx.Done():
		// If storage is itself stuck, startup still treats the durable running
		// record as interrupted. Never wait beyond the service manager's budget.
		log.Printf("shutdown could not finish persisting publication status; recovery will require verification")
	}
}

// Queued work must be cancelable even while another operation holds a mutex.
func lockContext(ctx context.Context, mu *sync.Mutex) bool {
	if ctx.Err() != nil {
		return false
	}
	if mu.TryLock() {
		return true
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if mu.TryLock() {
				if ctx.Err() != nil {
					mu.Unlock()
					return false
				}
				return true
			}
		}
	}
}
