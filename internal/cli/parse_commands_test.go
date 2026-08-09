package cli

import "testing"

func TestParseCommands(t *testing.T) {
	for _, args := range [][]string{{"build", "."}, {"check", "run_1"}, {"watch", "run_1"}, {"projects"}, {"machines"}, {"setup", "init"}, {"logs", "run_1"}} {
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
