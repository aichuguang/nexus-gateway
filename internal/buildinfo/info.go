// Package buildinfo is shared by CLI and HTTP build identification. Vite reads
// version.txt directly, so the UI and server cannot silently name two releases.
package buildinfo

import (
	_ "embed"
	"strings"
)

//go:embed version.txt
var versionText string

var Version = strings.TrimSpace(versionText)

// These values are set by scripts/build.mjs using Go linker flags. Ordinary
// development builds deliberately say unknown instead of inventing provenance.
var Commit = "unknown"
var BuildTime = "unknown"
var Dirty = "unknown"

type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"buildTime"`
	Dirty     string `json:"dirty"`
}

func Current() Info {
	return Info{Version: Version, Commit: Commit, BuildTime: BuildTime, Dirty: Dirty}
}
