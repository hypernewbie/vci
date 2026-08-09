package config

import "testing"

const valid = `schema_version = 1

[log_limits]
stdout_bytes = 100
stderr_bytes = 100

[retention]
max_bytes = 1000

[machines.mac-local]

[projects.Vci]
machines = ["mac-local"]
command = ["go", "test", "./..."]
`

func TestDecodeValidConfig(t *testing.T) {
	cfg, err := Decode([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Projects["Vci"].Command[0] != "go" {
		t.Fatalf("command: %#v", cfg.Projects["Vci"].Command)
	}
}

func TestDecodeRejectsUnknownFields(t *testing.T) {
	if _, err := Decode([]byte(valid + "\nunknown = true\n")); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestDecodeRejectsRemovedFields(t *testing.T) {
	for _, field := range []string{"total_bytes = 200", "max_runs = 10", "ttl_days = 2", "transport = \"local\"", "runtime = \"local\"", "host = \"builder\"", "os = \"darwin\"", "arch = \"arm64\"", "image = \"image\"", "snapshot = \"snapshot\""} {
		data := valid + "\n" + field + "\n"
		if _, err := Decode([]byte(data)); err == nil {
			t.Fatalf("removed field accepted: %s", field)
		}
	}
}

func TestValidateRejectsBrokenReferences(t *testing.T) {
	cfg, err := Decode([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Projects["Vci"] = Project{Machines: []string{"missing"}, Command: []string{"go"}}
	if err := Validate(cfg); err == nil {
		t.Fatal("missing machine accepted")
	}
}

// validEnvConfig is a coordinator root whose project declares a
// POSIX-identifier environment table with underscore keys.
const validEnvConfig = `schema_version = 1
orchestrator = "self"

[log_limits]
stdout_bytes = 100
stderr_bytes = 100

[retention]
max_bytes = 1000

[machines.mac-local]

[projects.Vci]
machines = ["mac-local"]
command = ["go", "test", "./..."]

[projects.Vci.environment]
FOO_BAR = "1"
_LEADING = "2"
Trailing_9 = "3"
`

// TestDecodeAcceptsValidEnvironmentKeys pins that underscore-bearing
// POSIX identifier keys decode and round-trip through validation.
func TestDecodeAcceptsValidEnvironmentKeys(t *testing.T) {
	cfg, err := Decode([]byte(validEnvConfig))
	if err != nil {
		t.Fatalf("valid environment keys rejected: %v", err)
	}
	env := cfg.Projects["Vci"].Environment
	if env["FOO_BAR"] != "1" || env["_LEADING"] != "2" || env["Trailing_9"] != "3" {
		t.Fatalf("environment round-trip: %#v", env)
	}
}

// TestDecodeRejectsInvalidEnvironmentKeys pins that any key outside
// `^[A-Za-z_][A-Za-z0-9_]*$` fails Decode (the validation entry
// point used before setup and run) as a configuration error. Quoted
// TOML keys keep punctuation and spaces as a single literal key so
// the rejection is produced by the identifier rule, not by TOML
// syntax.
func TestDecodeRejectsInvalidEnvironmentKeys(t *testing.T) {
	for _, key := range []string{"FOO-BAR", "1FOO", "FOO BAR", "FOO.BAR", "FOO:BAR", ""} {
		data := valid + "\n[projects.Vci.environment]\n\"" + key + "\" = \"x\"\n"
		if _, err := Decode([]byte(data)); err == nil {
			t.Fatalf("invalid environment key %q accepted", key)
		}
	}
}

// TestValidateProjectEnvironment exercises the identifier rule
// directly: valid underscore keys pass, invalid shapes are rejected.
func TestValidateProjectEnvironment(t *testing.T) {
	valid := map[string]string{"FOO": "1", "foo_bar": "2", "_LEAD": "3", "A": "4", "_": "5", "z9_": "6"}
	if err := ValidateProjectEnvironment("Vci", valid); err != nil {
		t.Fatalf("valid keys rejected: %v", err)
	}
	for _, key := range []string{"", "1FOO", "FOO-BAR", "FOO BAR", "FOO.BAR", "FOO:BAR"} {
		if err := ValidateProjectEnvironment("Vci", map[string]string{key: "x"}); err == nil {
			t.Fatalf("invalid key %q accepted", key)
		}
	}
}

// TestDecodeAcceptsMachineSourcePaths pins that a machine source_paths
// sub-table referencing a declared project decodes and round-trips.
func TestDecodeAcceptsMachineSourcePaths(t *testing.T) {
	data := valid + `
[machines.mac-local.source_paths]
Vci = "/Users/x/code/vci"
`
	cfg, err := Decode([]byte(data))
	if err != nil {
		t.Fatalf("valid source path rejected: %v", err)
	}
	if got := cfg.Machines["mac-local"].SourcePaths["Vci"]; got != "/Users/x/code/vci" {
		t.Fatalf("source path round-trip: %q", got)
	}
}

// TestDecodeRejectsUnknownSourcePathProject pins that a source_paths key
// must reference a declared project.
func TestDecodeRejectsUnknownSourcePathProject(t *testing.T) {
	data := valid + `
[machines.mac-local.source_paths]
Missing = "/tmp/seed"
`
	if _, err := Decode([]byte(data)); err == nil {
		t.Fatal("source path referencing unknown project accepted")
	}
}

// TestDecodeRejectsEmptySourcePath pins that empty source path values fail.
func TestDecodeRejectsEmptySourcePath(t *testing.T) {
	data := valid + `
[machines.mac-local.source_paths]
Vci = ""
`
	if _, err := Decode([]byte(data)); err == nil {
		t.Fatal("empty source path accepted")
	}
}

// TestDecodeRejectsControlCharSourcePath pins that control characters in a
// source path value fail validation.
func TestDecodeRejectsControlCharSourcePath(t *testing.T) {
	data := valid + `
[machines.mac-local.source_paths]
Vci = "/tmp/\u0001bad"
`
	if _, err := Decode([]byte(data)); err == nil {
		t.Fatal("control-character source path accepted")
	}
}

// TestDecodeRejectsNegativeBundleCache pins that any negative bundle-cache
// value fails Decode.
func TestDecodeRejectsNegativeBundleCache(t *testing.T) {
	for _, field := range []string{"max_entries = -1", "max_bytes = -1", "admission_bytes = -1"} {
		data := valid + "\n[machines.mac-local.bundle_cache]\n" + field + "\n"
		if _, err := Decode([]byte(data)); err == nil {
			t.Fatalf("negative bundle-cache field accepted: %s", field)
		}
	}
}

// TestDecodeAcceptsZeroBundleCache pins that an explicit zero policy decodes
// to the zero value, and that an omitted policy stays zero rather than being
// materialized at decode time.
func TestDecodeAcceptsZeroBundleCache(t *testing.T) {
	data := valid + `
[machines.mac-local.bundle_cache]
max_entries = 0
max_bytes = 0
admission_bytes = 0
`
	cfg, err := Decode([]byte(data))
	if err != nil {
		t.Fatalf("zero bundle-cache policy rejected: %v", err)
	}
	if bc := cfg.Machines["mac-local"].BundleCache; bc != (BundleCachePolicy{}) {
		t.Fatalf("zero bundle-cache round-trip: %#v", bc)
	}
}

// TestDefaultBundleCache pins the documented defaults and that omitting
// bundle_cache leaves the zero value for the consumer to resolve.
func TestDefaultBundleCache(t *testing.T) {
	def := DefaultBundleCache()
	if def.MaxEntries != 5 || def.MaxBytes != 5<<30 || def.AdmissionBytes != 50<<20 {
		t.Fatalf("unexpected default bundle cache: %#v", def)
	}
	cfg, err := Decode([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if bc := cfg.Machines["mac-local"].BundleCache; bc != (BundleCachePolicy{}) {
		t.Fatalf("omitted bundle_cache should stay zero: %#v", bc)
	}
}

// excludedPathConfig renders a minimal coordinator root whose Vci project
// carries the given excluded_paths list.
func excludedPathConfig(globs string) string {
	return `schema_version = 1

[log_limits]
stdout_bytes = 100
stderr_bytes = 100

[retention]
max_bytes = 1000

[machines.mac-local]

[projects.Vci]
machines = ["mac-local"]
command = ["go", "test", "./..."]
excluded_paths = ` + globs + "\n"
}

// TestDecodeAcceptsExcludedPaths pins that valid globs decode and round-trip.
func TestDecodeAcceptsExcludedPaths(t *testing.T) {
	cfg, err := Decode([]byte(excludedPathConfig(`["*.env", "secrets"]`)))
	if err != nil {
		t.Fatalf("valid excluded paths rejected: %v", err)
	}
	ep := cfg.Projects["Vci"].ExcludedPaths
	if len(ep) != 2 || ep[0] != "*.env" || ep[1] != "secrets" {
		t.Fatalf("excluded paths round-trip: %#v", ep)
	}
}

// TestDecodeRejectsReservedExcludedPath pins that .git and .vci cannot be
// workspace exclusions.
func TestDecodeRejectsReservedExcludedPath(t *testing.T) {
	for _, g := range []string{`[".git"]`, `[".vci"]`} {
		if _, err := Decode([]byte(excludedPathConfig(g))); err == nil {
			t.Fatalf("reserved excluded path accepted: %s", g)
		}
	}
}

// TestDecodeRejectsAbsoluteExcludedPath pins that absolute exclusions fail.
func TestDecodeRejectsAbsoluteExcludedPath(t *testing.T) {
	if _, err := Decode([]byte(excludedPathConfig(`["/abs"]`))); err == nil {
		t.Fatal("absolute excluded path accepted")
	}
}

// TestDecodeRejectsParentRefExcludedPath pins that parent references fail.
func TestDecodeRejectsParentRefExcludedPath(t *testing.T) {
	if _, err := Decode([]byte(excludedPathConfig(`["../x"]`))); err == nil {
		t.Fatal("parent-reference excluded path accepted")
	}
}

// TestDecodeRejectsMalformedExcludedGlob pins that uncompilable globs fail.
func TestDecodeRejectsMalformedExcludedGlob(t *testing.T) {
	if _, err := Decode([]byte(excludedPathConfig(`["["]`))); err == nil {
		t.Fatal("malformed excluded glob accepted")
	}
}

// TestValidateExcludedPaths exercises the shape rules directly: valid globs
// pass, reserved/absolute/parent/malformed/empty entries are rejected.
func TestValidateExcludedPaths(t *testing.T) {
	if err := ValidateExcludedPaths("Vci", nil); err != nil {
		t.Fatalf("empty glob list rejected: %v", err)
	}
	for _, glob := range []string{"*.env", "secrets"} {
		if err := ValidateExcludedPaths("Vci", []string{glob}); err != nil {
			t.Fatalf("valid glob %q rejected: %v", glob, err)
		}
	}
	for _, glob := range []string{".git", "/abs", "..", "[", ""} {
		if err := ValidateExcludedPaths("Vci", []string{glob}); err == nil {
			t.Fatalf("invalid glob %q accepted", glob)
		}
	}
}
