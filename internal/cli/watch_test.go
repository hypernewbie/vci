package cli

import (
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/model"
)

func TestParseWatchArgs(t *testing.T) {
	id, interval, exitStatus, err := parseWatchArgs([]string{"run_1", "--interval", "7", "--exit-status"})
	if err != nil {
		t.Fatal(err)
	}
	if id != "run_1" || interval != 7*time.Second || !exitStatus {
		t.Fatalf("got id=%q interval=%s exit-status=%v", id, interval, exitStatus)
	}
	if _, _, _, err := parseWatchArgs([]string{"run_1", "--interval", "0"}); err == nil {
		t.Fatal("expected invalid interval")
	}
	if _, _, _, err := parseWatchArgs([]string{"run_1", "--unknown"}); err == nil {
		t.Fatal("expected unknown flag failure")
	}
}

func TestWatchState(t *testing.T) {
	state, ok := watchState(map[string]any{"state": string(model.RunRunning)})
	if !ok || state != model.RunRunning {
		t.Fatalf("got state=%q ok=%v", state, ok)
	}
	if _, ok := watchState(map[string]any{"state": "unknown"}); ok {
		t.Fatal("accepted invalid state")
	}
}
