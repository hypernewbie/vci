package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/BurntSushi/toml"
)

func acquire(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create config directory: %w", err)
	}
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open config lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock config: %w", err)
	}
	return file, nil
}
func release(file *os.File) { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN); _ = file.Close() }

func Save(path string, cfg Config) error {
	if err := Validate(cfg); err != nil {
		return err
	}
	file, err := acquire(path)
	if err != nil {
		return err
	}
	defer release(file)
	return saveLocked(path, cfg)
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
