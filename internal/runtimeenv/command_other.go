//go:build !linux

package runtimeenv

import "os/exec"

func configureCommand(cmd *exec.Cmd) {}
