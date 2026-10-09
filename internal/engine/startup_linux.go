package engine

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

func proxyQuitSignal() os.Signal { return syscall.SIGQUIT }

// Merely finding a live PID never establishes ownership. Reuse requires the
// actual nginx executable and a master title naming this exact configuration;
// EnsureRunning then checks the file and the live workers' generation as well.
// Even a fully verified existing process is never signalled by RunningProxy.
func inspectNginxMaster(pid int, configPath, nginxBinary string) (bool, error) {
	if _, err := os.Stat("/proc/self"); err != nil {
		return false, fmt.Errorf("readable procfs is required to verify an existing nginx PID: %w", err)
	}
	proc := filepath.Join("/proc", strconv.Itoa(pid))
	cmdline, err := os.ReadFile(filepath.Join(proc, "cmdline"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("cannot verify existing nginx process: %w", err)
	}
	if !nginxMasterConfigMatches(cmdline, configPath) {
		return false, fmt.Errorf("managed PID belongs to an unverified process or another nginx configuration; no process was changed")
	}
	bin, err := exec.LookPath(nginxBinary)
	if err != nil {
		return false, fmt.Errorf("cannot resolve nginx executable for process verification: %w", err)
	}
	expected, err := os.Stat(bin)
	if err != nil {
		return false, err
	}
	actual, err := os.Stat(filepath.Join(proc, "exe"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("cannot verify existing nginx executable: %w", err)
	}
	if !os.SameFile(actual, expected) {
		return false, fmt.Errorf("managed PID is not the configured nginx executable; no process was changed")
	}
	return true, nil
}
