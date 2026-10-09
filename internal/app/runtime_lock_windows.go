package app

import (
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
)

func AcquireRuntimeLock(dataDir string) (func(), error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dataDir, "runtime.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	overlap := new(windows.Overlapped)
	if err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlap); err != nil {
		f.Close()
		return nil, fmt.Errorf("runtime is in use; stop the manager before offline recovery: %w", err)
	}
	return func() { _ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, overlap); _ = f.Close() }, nil
}

// Windows cannot fsync a directory with os.File; Linux is the deployment target.
func syncRuntimeDirectory(path string) error { return nil }

// Production recovery runs on Linux. A development PID file is conservatively
// treated as live until the operator stops/removes that unmanaged test fixture.
func runtimeProxyAlive(dataDir string) (bool, error) {
	_, err := os.Lstat(filepath.Join(dataDir, "nginx.pid"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
