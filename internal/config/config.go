package config

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode"

	"github.com/BurntSushi/toml"
	"github.com/hypernewbie/vci/internal/layout"
)

const SchemaVersion = 1

// OrchestratorSelf selects the local coordinator role. Any other non-empty
// value is treated as an SSH host passed to the ordinary system `ssh`
// executable by a client root.
const OrchestratorSelf = "self"

// Config is the on-disk Vci root configuration. The schema has exactly two
// roles: a coordinator (Orchestrator == OrchestratorSelf) that owns machine,
// project, retention, and log-limit policy, and a client (any other value)
// that holds only the orchestrator selector.
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
	// SourceCacheBytes optionally overrides the documented default
	// cache quota. Omitted means the documented default. The value
	// cannot be set on a client root.
	SourceCacheBytes int64 `toml:"source_cache_bytes"`
}

// Machine is the coordinator-owned inventory entry. Transport endpoints are
// not part of the machine definition; the orchestrator selector above is the
// only place a remote target appears. MaxConcurrent is the optional
// local-slot capacity for coordinator-owned parallel runs on this machine.
// Zero is the compatibility default of one slot.
type Machine struct {
	MaxConcurrent int `toml:"max_concurrent" json:"max_concurrent,omitempty"`
}

// EffectiveCapacity returns the local-slot capacity of a machine. Zero
// or omitted means one slot for compatibility with the original
// single-machine configuration. Negative values are rejected by
// coordinator validation.
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
}

// DefaultLogLimits and DefaultRetention are the coordinator defaults used
// when a fresh root is initialized. They are also the implicit values for a
// client root so that an omitted section decodes as the default without
// declaring coordinator policy.
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
		// Compatibility for existing local roots that pre-date the
		// orchestrator selector. Missing means self.
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

// Validate enforces role-aware configuration rules. A coordinator root may
// declare machines, projects, retention, and log limits. A client root may
// declare only the orchestrator selector; any coordinator-owned field is
// rejected because it could drift from the coordinator's authoritative
// state.
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

// ValidateOrchestrator rejects empty, option-like, whitespace, or control-
// character values. The string is passed to the system `ssh` executable, so
// it must be safe as a destination argument.
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
		if !layout.ValidName(name) {
			return fmt.Errorf("invalid machine name %q", name)
		}
		if machine.MaxConcurrent < 0 {
			return fmt.Errorf("machine %q has negative max_concurrent", name)
		}
	}
	for name, project := range cfg.Projects {
		if !layout.ValidName(name) {
			return fmt.Errorf("invalid project name %q", name)
		}
		if len(project.Machines) == 0 {
			return fmt.Errorf("project %q has no machines", name)
		}
		if len(project.Command) == 0 || strings.TrimSpace(project.Command[0]) == "" {
			return fmt.Errorf("project %q has no command", name)
		}
		seen := map[string]bool{}
		for _, machine := range project.Machines {
			if !layout.ValidName(machine) {
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

func formatKeys(keys []toml.Key) string {
	parts := make([]string, len(keys))
	for i, key := range keys {
		parts[i] = key.String()
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}
