package config

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestSaveRoundTripsAndProtectsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	cfg, err := Decode([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Projects["Vci"].Command[0] != "go" {
		t.Fatalf("loaded: %#v", loaded)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode %o, want 600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("unexpected files: %#v", entries)
	}
	for _, entry := range entries {
		if entry.Name() != "config.toml" && entry.Name() != "config.toml.lock" {
			t.Fatalf("unexpected file: %s", entry.Name())
		}
	}
}

func TestMutateSerializesCompatibleUpdates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := Save(path, Defaults()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- Mutate(path, func(cfg *Config) error {
				name := "machine-" + string(rune('a'+i))
				cfg.Machines[name] = Machine{}
				return nil
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Machines) != 8 {
		t.Fatalf("machines: %d", len(cfg.Machines))
	}
}

func TestSavePreservesPreviousConfigOnValidationFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	cfg, err := Decode([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	cfg.SchemaVersion = 999
	if err := Save(path, cfg); err == nil {
		t.Fatal("invalid config saved")
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("previous config lost: %v", err)
	}
}
