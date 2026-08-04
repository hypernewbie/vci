package app

import (
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
	if err := UpdateProject(l, "Vci", config.Project{Machines: []string{"mac-local"}, Command: []string{"go", "test"}}); err != nil {
		t.Fatal(err)
	}
	if err := RemoveProject(l, "Vci"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveMachine(l, "mac-local"); err != nil {
		t.Fatal(err)
	}
}
