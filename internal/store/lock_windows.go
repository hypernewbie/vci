//go:build windows

package store

import (
	"fmt"
	"os"
	"path/filepath"
)

func Acquire(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	// No flock on Windows. Coordinator not supported; client uses store
	// only for local checkout paths that are single-writer.
	return func() {
		_ = file.Close()
	}, nil
}
