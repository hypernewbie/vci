package lease

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/lock"
	"github.com/hypernewbie/vci/internal/model"
)

type Lease struct {
	RunID     model.RunID `json:"run_id"`
	Owner     string      `json:"owner"`
	ExpiresAt time.Time   `json:"expires_at"`
}

func Claim(l layout.Layout, id model.RunID, owner string, now time.Time, ttl time.Duration) error {
	dir, err := l.RunDir(string(id))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	unlock, err := lock.Acquire(filepath.Join(dir, "run.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	path := filepath.Join(dir, "lease.json")
	if existing, err := read(path); err == nil && existing.ExpiresAt.After(now) {
		return fmt.Errorf("run %s is leased by %s", id, existing.Owner)
	}
	data, err := json.Marshal(Lease{RunID: id, Owner: owner, ExpiresAt: now.Add(ttl).UTC()})
	if err != nil {
		return err
	}
	return atomicWrite(path, data)
}

func Renew(l layout.Layout, id model.RunID, owner string, now time.Time, ttl time.Duration) error {
	path, err := leasePath(l, id)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	unlock, err := lock.Acquire(filepath.Join(dir, "run.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	current, err := read(path)
	if err != nil {
		return err
	}
	if current.Owner != owner || !current.ExpiresAt.After(now) {
		return fmt.Errorf("lease for %s is not owned by %s", id, owner)
	}
	current.ExpiresAt = now.Add(ttl).UTC()
	data, err := json.Marshal(current)
	if err != nil {
		return err
	}
	return atomicWrite(path, data)
}

func Release(l layout.Layout, id model.RunID, owner string) error {
	path, err := leasePath(l, id)
	if err != nil {
		return err
	}
	unlock, err := lock.Acquire(filepath.Join(filepath.Dir(path), "run.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	current, err := read(path)
	if err != nil {
		return err
	}
	if current.Owner != owner {
		return fmt.Errorf("lease for %s is not owned by %s", id, owner)
	}
	return os.Remove(path)
}

func Read(l layout.Layout, id model.RunID) (Lease, error) {
	path, err := leasePath(l, id)
	if err != nil {
		return Lease{}, err
	}
	return read(path)
}

// ReadHasNoLease reports whether the named run has no worker lease
// (the lease file is absent). A corrupt lease file is treated as
// "has a lease" so the caller does not misclassify it as missing.
func ReadHasNoLease(l layout.Layout, id model.RunID) bool {
	path, err := leasePath(l, id)
	if err != nil {
		return false
	}
	if _, err := os.Stat(path); err != nil {
		return os.IsNotExist(err)
	}
	return false
}

func leasePath(l layout.Layout, id model.RunID) (string, error) {
	dir, err := l.RunDir(string(id))
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "lease.json"), nil
}
func read(path string) (Lease, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Lease{}, err
	}
	var out Lease
	if err := json.Unmarshal(data, &out); err != nil {
		return Lease{}, err
	}
	return out, nil
}

func atomicWrite(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".lease-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
