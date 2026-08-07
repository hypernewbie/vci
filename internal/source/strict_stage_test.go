package source

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hypernewbie/vci/internal/process"
)

// fakeRunner is a process.Runner that returns a fixed stdout for a
// given argv shape. Strict-stage tests use it to inject malformed
// stage records without relying on real Git state.
//
// Calls records every Run invocation so hosted-checkout tests can
// assert the exact command sequence, env, and dir. Existing tests
// that only inspect argv continue to work — the field is additive.
type fakeRunner struct {
	patterns []fakePattern
	failures []fakeFailure
	calls    []process.Command
}

type fakePattern struct {
	match  func(args []string) bool
	stdout string
}

type fakeFailure struct {
	match func(args []string) bool
	msg   string
}

func (f *fakeRunner) Run(ctx context.Context, cmd process.Command) (process.Result, error) {
	f.calls = append(f.calls, cmd)
	for _, fail := range f.failures {
		if fail.match(cmd.Args) {
			return process.Result{ExitCode: 1}, fmt.Errorf("fake runner: %s", fail.msg)
		}
	}
	for _, p := range f.patterns {
		if p.match(cmd.Args) {
			if cmd.Stdout != nil {
				_, _ = cmd.Stdout.Write([]byte(p.stdout))
			}
			return process.Result{ExitCode: 0}, nil
		}
	}
	return process.Result{ExitCode: 1}, fmt.Errorf("fake runner: no pattern for args %v", cmd.Args)
}

func matchExact(args []string) func([]string) bool {
	return func(got []string) bool {
		if len(got) != len(args) {
			return false
		}
		for i := range got {
			if got[i] != args[i] {
				return false
			}
		}
		return true
	}
}

func matchPrefix(prefix string) func([]string) bool {
	return func(args []string) bool {
		return len(args) > 0 && args[0] == prefix
	}
}

func matchHas(s string) func([]string) bool {
	return func(args []string) bool {
		for _, a := range args {
			if a == s {
				return true
			}
		}
		return false
	}
}

// TestListGitlinksStrictlyRejectsMalformedStage pins that malformed
// stage records (no tab, missing fields, non-zero stage, duplicate
// path, empty path) all fail closed with ErrSubmoduleUnavailable.
func TestListGitlinksStrictlyRejectsMalformedStage(t *testing.T) {
	cases := []struct {
		name   string
		stdout string
	}{
		{"no_tab", "160000 abc 0 no_tab_path"},
		{"missing_field", "160000 abc\tpath"},
		{"non_zero_stage", "160000 abc 1\tstaged_path"},
		{"empty_path", "160000 abc 0\t"},
		{"duplicate_path", "160000 abc 0\tdup\x00160000 def 0\tdup"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{patterns: []fakePattern{
				{match: matchHas("ls-files"), stdout: tc.stdout},
			}}
			_, err := listGitlinks(context.Background(), "/anywhere", runner)
			if err == nil {
				t.Fatalf("malformed stage %q must fail", tc.name)
			}
			if !errors.Is(err, ErrSubmoduleUnavailable) {
				t.Fatalf("want ErrSubmoduleUnavailable, got %v", err)
			}
		})
	}
}

// TestListGitlinksAcceptsValidRecord pins that a single well-formed
// 160000 stage record is returned verbatim.
func TestListGitlinksAcceptsValidRecord(t *testing.T) {
	runner := &fakeRunner{patterns: []fakePattern{
		{match: matchHas("ls-files"), stdout: "160000 abc 0\tchild"},
	}}
	links, err := listGitlinks(context.Background(), "/anywhere", runner)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0] != "child" {
		t.Fatalf("links: %v", links)
	}
}

// TestListGitlinksIgnoresNonGitlinks pins that non-160000 records
// (regular files, executable files, symlinks) are not surfaced as
// gitlinks. The collection step is the only place a verification
// trigger is needed.
func TestListGitlinksIgnoresNonGitlinks(t *testing.T) {
	stdout := "100644 abc 0\tfile\x00100755 def 0\texec\x00120000 ghi 0\tsymlink"
	runner := &fakeRunner{patterns: []fakePattern{
		{match: matchHas("ls-files"), stdout: stdout},
	}}
	links, err := listGitlinks(context.Background(), "/anywhere", runner)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 0 {
		t.Fatalf("non-gitlink records must be ignored; got %v", links)
	}
}

// TestValidateInputRejectsTrailingSlash pins that a trailing slash
// is rejected by the canonical entry validator. The supported
// directory shape is one entry without a trailing slash.

// TestCanonicalEntryRejectsEmptySegment pins that a path with an
// empty segment (e.g. "foo//bar") is rejected.
func TestCanonicalEntryRejectsEmptySegment(t *testing.T) {
	_, err := canonicalEntry("/tmp", "foo//bar")
	if err == nil {
		t.Fatal("empty segment must be rejected")
	}
}

// TestCanonicalEntryAcceptsTopLevelFile pins that a top-level file
// is accepted.
func TestCanonicalEntryAcceptsTopLevelFile(t *testing.T) {
	out, err := canonicalEntry("/tmp", "README.md")
	if err != nil {
		t.Fatal(err)
	}
	if out != "README.md" {
		t.Fatalf("output: %q", out)
	}
}

// TestCanonicalEntryRejectsTrailingSlash pins that a trailing slash
// is rejected as a malformed entry.
func TestCanonicalEntryRejectsTrailingSlash(t *testing.T) {
	_, err := canonicalEntry("/tmp", "child/")
	if err == nil {
		t.Fatal("trailing slash must be rejected")
	}
	if !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("error: %v", err)
	}
}

func contains(items []string, want string) bool {
	for _, x := range items {
		if x == want {
			return true
		}
	}
	return false
}
