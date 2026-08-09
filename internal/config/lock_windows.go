//go:build windows

package config

import (
	"fmt"
	"os"
	"path/filepath"
)

func acquire(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create config directory: %w", err)
	}
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open config lock: %w", err)
	}
	// No flock on Windows: client-only build, single writer.
	// Coordinator is not supported on Windows.
	return file, nil
}

func release(file *os.File) { _ = file.Close() }
