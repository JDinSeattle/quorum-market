// Package obs holds the operational surface every service shares: metrics,
// request-scoped logging, readiness checks, and the admin endpoint that
// exposes them.
package obs

import (
	"runtime"
	"runtime/debug"
)

// Build metadata, stamped at link time:
//
//	-ldflags "-X github.com/JDinSeattle/quorum-market/internal/obs.version=1.4.0 ..."
//
// Defaults describe an unstamped local build so a developer binary is never
// mistaken for a released one.
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

// BuildInfo describes the running binary.
type BuildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"buildDate"`
	GoVersion string `json:"goVersion"`
	Platform  string `json:"platform"`
}

// Build returns the running binary's provenance. When the link-time values are
// absent it falls back to the VCS stamps the Go toolchain embeds
// automatically, so `go build` binaries still report their commit.
func Build() BuildInfo {
	info := BuildInfo{
		Version:   version,
		Commit:    commit,
		BuildDate: date,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}

	if info.Commit != "unknown" && info.BuildDate != "unknown" {
		return info
	}
	buildInfo, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}
	for _, setting := range buildInfo.Settings {
		switch setting.Key {
		case "vcs.revision":
			if info.Commit == "unknown" {
				info.Commit = setting.Value
			}
		case "vcs.time":
			if info.BuildDate == "unknown" {
				info.BuildDate = setting.Value
			}
		case "vcs.modified":
			if setting.Value == "true" {
				info.Version += "-dirty"
			}
		}
	}
	return info
}
