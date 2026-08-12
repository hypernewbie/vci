package version

import (
	"runtime/debug"
	"testing"
)

func TestCurrentUsesEmbeddedDevelopmentVersion(t *testing.T) {
	restore := saveMetadataHooks()
	defer restore()
	releaseVersion = ""
	releaseCommit = ""
	releaseDate = ""
	runtimeBuildInfo = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, true
	}

	got := Current()
	if got.Version != "0.1.0-dev" {
		t.Fatalf("version = %q, want 0.1.0-dev", got.Version)
	}
	if got.Commit != "unknown" || got.BuildDate != "unknown" {
		t.Fatalf("development metadata = %+v", got)
	}
	if got.GoVersion == "" || got.OS == "" || got.Arch == "" {
		t.Fatalf("incomplete runtime metadata = %+v", got)
	}
}

func TestCurrentMetadataPrecedence(t *testing.T) {
	restore := saveMetadataHooks()
	defer restore()
	build := &debug.BuildInfo{
		Main: debug.Module{Version: "v0.9.0"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "build-info-commit"},
			{Key: "vcs.time", Value: "2026-01-02T03:04:05Z"},
		},
	}
	runtimeBuildInfo = func() (*debug.BuildInfo, bool) { return build, true }
	releaseVersion = "v0.1.0"
	releaseCommit = "release-commit"
	releaseDate = "2026-02-03T04:05:06Z"

	got := Current()
	if got.Version != "0.1.0" || got.Commit != "release-commit" || got.BuildDate != "2026-02-03T04:05:06Z" {
		t.Fatalf("release metadata did not win: %+v", got)
	}

	releaseVersion = ""
	releaseCommit = ""
	releaseDate = ""
	got = Current()
	if got.Version != "0.9.0" || got.Commit != "build-info-commit" || got.BuildDate != "2026-01-02T03:04:05Z" {
		t.Fatalf("build metadata did not win fallback: %+v", got)
	}
}

func TestValidSemanticVersion(t *testing.T) {
	for _, value := range []string{"0.1.0", "1.2.3", "1.2.3-rc.1", "1.2.3+build.7", "1.2.3-rc.1+build.7"} {
		if !Valid(value) {
			t.Errorf("Valid(%q) = false", value)
		}
	}
	for _, value := range []string{"v0.1.0", "0.1", "01.2.3", "1.02.3", "1.2.03", "", "(devel)"} {
		if Valid(value) {
			t.Errorf("Valid(%q) = true", value)
		}
	}
}

func saveMetadataHooks() func() {
	oldRuntimeBuildInfo := runtimeBuildInfo
	oldReleaseVersion := releaseVersion
	oldReleaseCommit := releaseCommit
	oldReleaseDate := releaseDate
	return func() {
		runtimeBuildInfo = oldRuntimeBuildInfo
		releaseVersion = oldReleaseVersion
		releaseCommit = oldReleaseCommit
		releaseDate = oldReleaseDate
	}
}
