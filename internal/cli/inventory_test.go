package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestSetupAndInventoryCommands(t *testing.T) {
	t.Setenv("VCI_ROOT", filepath.Join(t.TempDir(), ".vci"))
	var out, errOut bytes.Buffer
	if code := Run([]string{"setup", "init"}, &out, &errOut); code != 0 {
		t.Fatalf("setup: %d %s", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"machines"}, &out, &errOut); code != 0 {
		t.Fatalf("machines: %d %s", code, out.String())
	}
	var response Response
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK {
		t.Fatalf("machines failed: %+v", response.Error)
	}
}
