package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/layout"
)

func testLayout(t *testing.T) layout.Layout {
	t.Helper()
	l := layout.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := Initialize(l); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestMachineAndProjectLifecycle(t *testing.T) {
	l := testLayout(t)
	if err := AddMachine(l, "mac-local", config.Machine{}); err != nil {
		t.Fatal(err)
	}
	if err := AddProject(l, "Vci", config.Project{Machines: []string{"mac-local"}, Command: []string{"go", "test", "./..."}}); err != nil {
		t.Fatal(err)
	}
	inventory, err := ReadInventory(l)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Machines) != 1 || len(inventory.Projects) != 1 {
		t.Fatalf("inventory: %+v", inventory)
	}
	if err := RemoveMachine(l, "mac-local"); err == nil {
		t.Fatal("removed attached machine")
	}
}

// TestInventoryPropagatesSchedulerStatusError pins that a scheduler
// inspection failure (for example a non-directory at the scheduler
// lock parent) is propagated to ReadInventory. Today the inventory
// silently suppresses the error and fabricates `available == capacity`,
// which falsely reports free slots to the operator.
func TestInventoryPropagatesSchedulerStatusError(t *testing.T) {
	l := testLayout(t)
	if err := AddMachine(l, "alpha", config.Machine{}); err != nil {
		t.Fatal(err)
	}
	if err := AddProject(l, "demo", config.Project{Machines: []string{"alpha"}, Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	// Replace the scheduler lock parent with a regular file so the
	// scheduler lock acquisition fails. ReadInventory must surface the
	// failure rather than fabricate availability.
	if err := os.RemoveAll(l.LocksDir()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(l.LocksDir()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.LocksDir(), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInventory(l); err == nil {
		t.Fatal("ReadInventory must propagate scheduler inspection failure")
	}
}
