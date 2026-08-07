package executor

import (
	"bytes"
	"context"
	"testing"
)

func TestLocalCapturesJobExit(t *testing.T) {
	var out, errOut bytes.Buffer
	result, err := (Local{}).ExecuteSupervised(context.Background(), Request{Executable: "/bin/sh", Args: []string{"-c", "printf ok; printf bad >&2; exit 7"}, Workspace: t.TempDir(), Stdout: &out, Stderr: &errOut}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 7 || out.String() != "ok" || errOut.String() != "bad" {
		t.Fatalf("result=%+v out=%q err=%q", result, out.String(), errOut.String())
	}
}
