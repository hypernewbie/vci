package cli

import "testing"

func TestParseCommands(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"version", "--coordinator"}, {"build", "."}, {"check", "run_1"}, {"watch", "run_1"}, {"projects"}, {"machines"}, {"setup", "init"}, {"logs", "run_1"}, {"internal-stage", "~/.vci/state/work/run_1"}, {"internal-probe-cache", "~/.vci/state/bundle-cache/v1/Vci/abc"}, {"internal-acquire-claim", "~/.vci/state/bundle-cache/v1/Vci/abc", "run_1"}, {"internal-release-claim", "~/.vci/state/bundle-cache/v1/Vci/abc", "run_1"}, {"internal-reap-cache", "a", "b", "c", "d", "e", "f", "1", "2"}, {"internal-reconstruct", "~/.vci/state/work/run_1", "--no-seed"}} {
		got, err := Parse(args)
		if err != nil {
			t.Fatalf("Parse(%q): %v", args, err)
		}
		if got.Name != args[0] {
			t.Fatalf("name = %q, want %q", got.Name, args[0])
		}
	}
	if _, err := Parse(nil); err == nil || err.Code != "command_required" {
		t.Fatalf("missing command: %v", err)
	}
	for _, args := range [][]string{{"explode"}} {
		if _, err := Parse(args); err == nil || err.Code != "unknown_command" {
			t.Fatalf("unknown command %q: %v", args, err)
		}
	}
}
