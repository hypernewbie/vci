package app

import (
	"path/filepath"
	"testing"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/layout"
)

func TestInitializeIsIdempotent(t *testing.T) {
	l := layout.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := Initialize(l); err != nil {
		t.Fatal(err)
	}
	first, err := config.Load(l.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := Initialize(l); err != nil {
		t.Fatal(err)
	}
	second, err := config.Load(l.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if first.SchemaVersion != second.SchemaVersion {
		t.Fatalf("init changed config")
	}
}
