package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hypernewbie/vci/internal/lock"
	"github.com/hypernewbie/vci/internal/model"
)

func (s Store) PublishResult(id model.RunID, value any) error {
	dir, err := s.Layout.RunDir(string(id))
	if err != nil {
		return err
	}
	unlock, err := lock.Acquire(filepath.Join(dir, "run.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	record, err := s.loadPath(filepath.Join(dir, "run.json"), id)
	if err != nil {
		return err
	}
	if record.State == model.RunSucceeded || record.State == model.RunFailed || record.State == model.RunLost || record.State == model.RunAborted {
		return fmt.Errorf("run %s is terminal", id)
	}
	path := filepath.Join(dir, "result.json")
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("run %s already has a result", id)
	} else if !os.IsNotExist(err) {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return atomicJSON(path, data)
}

func (s Store) ReadResult(id model.RunID) (json.RawMessage, error) {
	dir, err := s.Layout.RunDir(string(id))
	if err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(dir, "result.json"))
}
