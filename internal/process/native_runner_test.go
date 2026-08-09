//go:build !windows

package process

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestNativeCancellationTerminatesTheOwnedGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan Running, 1)
	done := make(chan error, 1)
	go func() {
		_, err := (Native{}).RunSupervised(ctx, Command{Executable: "/bin/sh", Args: []string{"-c", "sleep 30"}}, func(r Running) error { started <- r; return nil })
		done <- err
	}()
	select {
	case running := <-started:
		if running.PID <= 0 || running.PGID <= 0 {
			t.Fatalf("running: %+v", running)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("process did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("owned group did not terminate")
	}
}

func TestNativePreservesArgumentBoundaries(t *testing.T) {
	var out strings.Builder
	result, err := (Native{}).Run(context.Background(), Command{Executable: "/bin/echo", Args: []string{"a b", "x;y"}, Stdout: &out})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("run: %+v %v", result, err)
	}
	if strings.TrimSpace(out.String()) != "a b x;y" {
		t.Fatalf("output: %q", out.String())
	}
}
