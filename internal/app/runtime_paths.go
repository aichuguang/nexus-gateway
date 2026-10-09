package app

import (
	"os"
	"strings"
)

const defaultBinaryPath = "/opt/gateway/gateway"

type runtimePaths struct {
	BinaryPath string `json:"binaryPath"`
	DataDir    string `json:"dataDir"`
}

// Cache this at startup so an atomic binary replacement cannot leave a
// Linux "(deleted)" executable path in certificate deployment commands.
func runningBinaryPath() string {
	path, err := os.Executable()
	if err != nil || strings.TrimSpace(path) == "" {
		return defaultBinaryPath
	}
	return path
}

func (a *App) runtimePaths() runtimePaths {
	binaryPath := a.binaryPath
	if binaryPath == "" {
		binaryPath = defaultBinaryPath
	}
	return runtimePaths{BinaryPath: binaryPath, DataDir: a.opts.DataDir}
}
