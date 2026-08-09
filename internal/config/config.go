package config

import (
	"bytes"
	"fmt"
	"github.com/BurntSushi/toml"
	"github.com/hypernewbie/vci/internal/model"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"unicode"
)

const SchemaVersion = 1

// OrchestratorSelf selects local coordination.
// Non-empty values are SSH hosts for clients.
const OrchestratorSelf = "self"

// Config is root config.
// Coordinator roots define machines, projects, and limits.
// Client roots only set the orchestrator.
type Config struct {
	SchemaVersion int                `toml:"schema_version"`
	Orchestrator  string             `toml:"orchestrator"`
	LogLimits     LogLimits          `toml:"log_limits"`
	Retention     Retention          `toml:"retention"`
	Machines      map[string]Machine `toml:"machines"`
	Projects      map[string]Project `toml:"projects"`
}

type LogLimits struct {
	StdoutBytes int64 `toml:"stdout_bytes"`
	StderrBytes int64 `toml:"stderr_bytes"`
}

type Retention struct {
	MaxBytes int64 `toml:"max_bytes"`
	// SourceCacheBytes overrides cache quota.
	// Omitted uses default and is disallowed on client roots.
	SourceCacheBytes int64 `toml:"source_cache_bytes"`
}

// Machine defines one executable slot.
// MaxConcurrent is local capacity; zero means one.
// Host is optional SSH destination; runtime selects host/docker/vm behavior.
type Machine struct {
	MaxConcurrent int    `toml:"max_concurrent" json:"max_concurrent,omitempty"`
	Host          string `toml:"host" json:"host,omitempty"`
	Runtime       string `toml:"runtime" json:"runtime,omitempty"`
	Image         string `toml:"image" json:"image,omitempty"`
	Snapshot      string `toml:"snapshot" json:"snapshot,omitempty"`
}

// EffectiveCapacity returns local slot capacity, treating 0 or missing as one.
func EffectiveCapacity(m Machine) int {
	if m.MaxConcurrent <= 0 {
		return 1
	}
	return m.MaxConcurrent
}

type Project struct {
	Machines    []string          `toml:"machines" json:"machines"`
	Command     []string          `toml:"command" json:"command"`
	Environment map[string]string `toml:"environment" json:"environment,omitempty"`
	// Artifacts are optional workspace-relative glob patterns.
	// Matching regular files are copied into run artifacts; symlinks, devices,
	// `..`, `.git`, and `.vci` entries are rejected.
	Artifacts []string `toml:"artifacts,omitempty" json:"artifacts,omitempty"`
	// HostedFallback is optional source data for `vci build --hosted`.
	// URL and Commit must both be set or both be empty.
	HostedFallback HostedFallback `toml:"hosted_fallback" json:"hosted_fallback,omitempty"`
}

// DefaultLogLimits and DefaultRetention are coordinator defaults for new roots
// and implicit client defaults when fields are omitted.
var (
	DefaultLogLimits = LogLimits{StdoutBytes: 4 << 20, StderrBytes: 4 << 20}
	DefaultRetention = Retention{MaxBytes: 512 << 20}
)

func Defaults() Config {
	return Config{
		SchemaVersion: SchemaVersion,
		Orchestrator:  OrchestratorSelf,
		LogLimits:     DefaultLogLimits,
		Retention:     DefaultRetention,
		Machines:      map[string]Machine{}, Projects: map[string]Project{},
	}
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	return Decode(data)
}

func Decode(data []byte) (Config, error) {
	cfg := Defaults()
	meta, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if !meta.IsDefined("orchestrator") {
		// Existing local roots without orchestrator are treated as self.
		cfg.Orchestrator = OrchestratorSelf
	}
	if undecoded := meta.Undecoded(); len(undecoded) != 0 {
		return Config{}, fmt.Errorf("unknown config fields: %s", formatKeys(undecoded))
	}
	if err := Validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate enforces role-aware configuration rules for coordinator and client roots.
func Validate(cfg Config) error {
	if cfg.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported config schema version %d", cfg.SchemaVersion)
	}
	if err := ValidateOrchestrator(cfg.Orchestrator); err != nil {
		return err
	}
	if cfg.Orchestrator == OrchestratorSelf {
		return validateCoordinator(cfg)
	}
	return validateClient(cfg)
}

// ValidateMachineHost checks machine hosts for ssh-safe destination format.
// Empty hosts are local; reject whitespace/control chars, leading '-', schemes,
// and .. segments.
func ValidateMachineHost(host string) error {
	if host == "" {
		return nil
	}
	if strings.HasPrefix(host, "-") {
		return fmt.Errorf("machine host %q looks like an option flag", host)
	}
	for _, r := range host {
		if unicode.IsSpace(r) || r < 0x20 || r == 0x7f {
			return fmt.Errorf("machine host %q contains whitespace or control characters", host)
		}
	}
	if strings.Contains(host, "://") {
		return fmt.Errorf("machine host %q must not use a scheme", host)
	}
	for _, segment := range strings.Split(host, "/") {
		if segment == ".." {
			return fmt.Errorf("machine host %q must not contain a .. segment", host)
		}
	}
	return nil
}

// ValidateOrchestrator validates an ssh destination string.
func ValidateOrchestrator(value string) error {
	if value == "" {
		return fmt.Errorf("orchestrator value is empty")
	}
	if strings.HasPrefix(value, "-") {
		return fmt.Errorf("orchestrator value %q looks like an option flag", value)
	}
	for _, r := range value {
		if unicode.IsSpace(r) || r < 0x20 || r == 0x7f {
			return fmt.Errorf("orchestrator value %q contains whitespace or control characters", value)
		}
	}
	return nil
}

func validateCoordinator(cfg Config) error {
	if cfg.LogLimits.StdoutBytes <= 0 || cfg.LogLimits.StderrBytes <= 0 {
		return fmt.Errorf("log limits are invalid")
	}
	if cfg.Retention.MaxBytes <= 0 {
		return fmt.Errorf("retention is invalid")
	}
	if cfg.Retention.SourceCacheBytes < 0 {
		return fmt.Errorf("source cache quota is invalid")
	}
	if cfg.Retention.SourceCacheBytes > 0 && cfg.Retention.SourceCacheBytes < 4096 {
		return fmt.Errorf("source cache quota is below minimum 4 KB")
	}
	for name, machine := range cfg.Machines {
		if !model.ValidName(name) {
			return fmt.Errorf("invalid machine name %q", name)
		}
		if machine.MaxConcurrent < 0 {
			return fmt.Errorf("machine %q has negative max_concurrent", name)
		}
		if err := ValidateMachineHost(machine.Host); err != nil {
			return err
		}
		if err := ValidateMachineRuntime(name, machine); err != nil {
			return err
		}
	}
	for name, project := range cfg.Projects {
		if !model.ValidName(name) {
			return fmt.Errorf("invalid project name %q", name)
		}
		if len(project.Machines) == 0 {
			return fmt.Errorf("project %q has no machines", name)
		}
		if len(project.Command) == 0 || strings.TrimSpace(project.Command[0]) == "" {
			return fmt.Errorf("project %q has no command", name)
		}
		if err := ValidateProjectEnvironment(name, project.Environment); err != nil {
			return err
		}
		// Hosted fallback is optional; if set, both fields must validate.
		if project.HostedFallback.URL != "" || project.HostedFallback.Commit != "" {
			if _, err := project.HostedFallback.Validate(); err != nil {
				return fmt.Errorf("project %q hosted fallback: %w", name, err)
			}
		}
		if err := ValidateProjectArtifacts(name, project.Artifacts); err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, machine := range project.Machines {
			if !model.ValidName(machine) {
				return fmt.Errorf("project %q has invalid machine %q", name, machine)
			}
			if seen[machine] {
				return fmt.Errorf("project %q repeats machine %q", name, machine)
			}
			seen[machine] = true
			if _, ok := cfg.Machines[machine]; !ok {
				return fmt.Errorf("project %q references missing machine %q", name, machine)
			}
		}
	}
	return nil
}

func validateClient(cfg Config) error {
	if len(cfg.Machines) != 0 {
		return fmt.Errorf("client root must not declare machines")
	}
	if len(cfg.Projects) != 0 {
		return fmt.Errorf("client root must not declare projects")
	}
	if cfg.Retention.SourceCacheBytes != 0 {
		return fmt.Errorf("client root must not set source-cache quota")
	}
	if cfg.Retention.MaxBytes != DefaultRetention.MaxBytes {
		return fmt.Errorf("client root must not set retention policy")
	}
	if cfg.LogLimits != DefaultLogLimits {
		return fmt.Errorf("client root must not set log limits")
	}
	return nil
}

// validEnvKey enforces POSIX env var names.
var validEnvKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidateProjectEnvironment checks that project env keys are POSIX identifiers.
func ValidateProjectEnvironment(name string, env map[string]string) error {
	for key := range env {
		if !validEnvKey.MatchString(key) {
			return fmt.Errorf("project %q environment key %q is not a valid POSIX identifier", name, key)
		}
	}
	return nil
}

// ValidateProjectArtifacts enforces allowed artifact glob format and safety.
func ValidateProjectArtifacts(name string, globs []string) error {
	for i, glob := range globs {
		if err := validateArtifactGlob(glob); err != nil {
			return fmt.Errorf("project %q artifact[%d]: %w", name, i, err)
		}
	}
	return nil
}

func validateArtifactGlob(glob string) error {
	if glob == "" {
		return fmt.Errorf("artifact glob is empty")
	}
	if strings.ContainsAny(glob, " \t\r\n\v\f\x00") {
		return fmt.Errorf("artifact glob %q contains whitespace or control characters", glob)
	}
	if strings.HasPrefix(glob, "-") {
		return fmt.Errorf("artifact glob %q starts with a flag-like character", glob)
	}
	if strings.Contains(glob, "://") {
		return fmt.Errorf("artifact glob %q must not use a scheme", glob)
	}
	if strings.HasPrefix(glob, "/") {
		return fmt.Errorf("artifact glob %q must not be an absolute path", glob)
	}
	for _, segment := range strings.Split(glob, "/") {
		if segment == ".." {
			return fmt.Errorf("artifact glob %q must not contain a .. segment", glob)
		}
	}
	if _, err := path.Match(glob, ""); err != nil {
		return fmt.Errorf("artifact glob %q is not a valid path.Match pattern: %w", glob, err)
	}
	return nil
}

func formatKeys(keys []toml.Key) string {
	parts := make([]string, len(keys))
	for i, key := range keys {
		parts[i] = key.String()
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func Initialize(path string) error {
	file, err := acquire(path)
	if err != nil {
		return err
	}
	defer release(file)
	if _, err := os.Stat(path); err == nil {
		if _, err := Load(path); err != nil {
			return fmt.Errorf("validate existing config: %w", err)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect config: %w", err)
	}
	return saveLocked(path, Defaults())
}

func Mutate(path string, fn func(*Config) error) error {
	file, err := acquire(path)
	if err != nil {
		return err
	}
	defer release(file)
	cfg, err := Load(path)
	if err != nil {
		return err
	}
	if err := fn(&cfg); err != nil {
		return err
	}
	return saveLocked(path, cfg)
}

func saveLocked(path string, cfg Config) error {
	if err := Validate(cfg); err != nil {
		return err
	}
	var encoded bytes.Buffer
	if err := toml.NewEncoder(&encoded).Encode(cfg); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create config temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("protect config temp file: %w", err)
	}
	if _, err := tmp.Write(encoded.Bytes()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write config temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync config temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close config temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("publish config: %w", err)
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync config directory: %w", err)
	}
	return nil
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil && runtime.GOOS != "windows" {
		return err
	}
	return nil
}
