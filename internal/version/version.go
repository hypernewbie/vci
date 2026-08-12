// Package version reports Vci's application version and build identity.
//
// Application SemVer is separate from the JSON, configuration, and durable
// state schema versions. Release builds set the release fields with ldflags;
// ordinary Go builds use module/VCS build information when it is available.
package version

import (
	_ "embed"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
)

// VERSION is the single tracked source of the next intended application
// release. Release tags and release linker flags must match it.
//
//go:embed VERSION
var embeddedVersion string

// The release workflow sets these variables. They remain variables rather than
// constants so Go's linker can replace them with -X.
var (
	releaseVersion = ""
	releaseCommit  = ""
	releaseDate    = ""
)

var semverPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

// Details is the stable machine-readable identity returned by `vci version`.
type Details struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
	GoVersion string `json:"go_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

// Current returns the identity of the running binary.
func Current() Details {
	info, ok := runtimeBuildInfo()
	if !ok {
		info = nil
	}
	return Details{
		Version:   resolvedVersion(info),
		Commit:    resolvedSetting(info, "vcs.revision", releaseCommit),
		BuildDate: resolvedSetting(info, "vcs.time", releaseDate),
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}
}

// runtimeBuildInfo is a variable so tests can exercise precedence without
// changing the production metadata source.
var runtimeBuildInfo = func() (*debug.BuildInfo, bool) {
	return debug.ReadBuildInfo()
}

func resolvedVersion(info *debug.BuildInfo) string {
	base := strings.TrimSpace(embeddedVersion)
	if !Valid(base) {
		base = "0.0.0"
	}
	if candidate := normalize(releaseVersion); Valid(candidate) {
		return candidate
	}
	if info != nil {
		if candidate := normalize(info.Main.Version); Valid(candidate) {
			return candidate
		}
	}
	return base + "-dev"
}

func resolvedSetting(info *debug.BuildInfo, key, override string) string {
	if value := strings.TrimSpace(override); value != "" {
		return value
	}
	if info != nil {
		for _, setting := range info.Settings {
			if setting.Key == key && strings.TrimSpace(setting.Value) != "" && setting.Value != "unknown" {
				return strings.TrimSpace(setting.Value)
			}
		}
	}
	return "unknown"
}

func normalize(value string) string {
	return strings.TrimPrefix(strings.TrimSpace(value), "v")
}

// Valid reports whether value is a strict semantic version without a leading
// `v`. It is used by release tooling and by the runtime metadata fallback.
func Valid(value string) bool {
	return semverPattern.MatchString(value)
}
