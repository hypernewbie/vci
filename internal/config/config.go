package config

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/hypernewbie/vci/internal/layout"
)

const SchemaVersion = 1

type Config struct {
	SchemaVersion int                `toml:"schema_version"`
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
}

type Machine struct{}

type Project struct {
	Machines    []string          `toml:"machines"`
	Command     []string          `toml:"command"`
	Environment map[string]string `toml:"environment"`
}

func Defaults() Config {
	return Config{
		SchemaVersion: SchemaVersion,
		LogLimits:     LogLimits{StdoutBytes: 4 << 20, StderrBytes: 4 << 20},
		Retention:     Retention{MaxBytes: 512 << 20},
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
	if undecoded := meta.Undecoded(); len(undecoded) != 0 {
		return Config{}, fmt.Errorf("unknown config fields: %s", formatKeys(undecoded))
	}
	if err := Validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func Validate(cfg Config) error {
	if cfg.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported config schema version %d", cfg.SchemaVersion)
	}
	if cfg.LogLimits.StdoutBytes <= 0 || cfg.LogLimits.StderrBytes <= 0 {
		return fmt.Errorf("log limits are invalid")
	}
	if cfg.Retention.MaxBytes <= 0 {
		return fmt.Errorf("retention is invalid")
	}
	for name := range cfg.Machines {
		if !layout.ValidName(name) {
			return fmt.Errorf("invalid machine name %q", name)
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

func formatKeys(keys []toml.Key) string {
	parts := make([]string, len(keys))
	for i, key := range keys {
		parts[i] = key.String()
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}
