package app

// Release script deadline regression test.
//
// The release gate scripts poll a real detached `go test ./...` job.
// The job compiles and runs the full Go suite, which takes well over
// 30 seconds on a warm cache; the pre-fix 300 x 0.1s (30s) poll bound
// therefore expired with the run still `running`. This test pins the
// scripts to a fixed, finite deadline large enough for the real job so
// the regression cannot silently return.
//
// The scripts are read-only fixtures of the repository; the test never
// executes them (execution happens in the release gate itself) and
// never writes to the repository.

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// releaseDeadlineFloor is the minimum acceptable bounded deadline in
// seconds. The real detached job measured ~50s warm; the floor leaves
// headroom for slower machines while staying finite.
const releaseDeadlineFloor = 240

var deadlineExpr = regexp.MustCompile(`deadline=\$\(\( \$\(date \+%s\) \+ ([0-9]+) \)\)`)

func TestReleaseScriptsHaveSufficientBoundedDeadline(t *testing.T) {
	for _, name := range []string{"self-check.sh", "detach-check.sh"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(repoRoot(t), "scripts", name)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			body := string(data)

			// The poll must be bounded by a fixed deadline in seconds.
			m := deadlineExpr.FindStringSubmatch(body)
			if m == nil {
				t.Fatalf("%s must define a bounded deadline with `$(date +%%s)`; got:\n%s", name, body)
			}
			seconds, err := strconv.Atoi(m[1])
			if err != nil {
				t.Fatalf("%s deadline is not numeric: %q", name, m[1])
			}
			if seconds < releaseDeadlineFloor {
				t.Fatalf("%s deadline %ds is below the %ds floor required by the real go test ./... job", name, seconds, releaseDeadlineFloor)
			}

			// No unbounded loop may exist alongside the deadline.
			for _, banned := range []string{"while true", "for ;;", "while :"} {
				if hasUnboundedLoop(body, banned) {
					t.Fatalf("%s must not contain an unbounded loop %q", name, banned)
				}
			}

			// A non-terminal state after the deadline must fail loudly.
			if !regexp.MustCompile(`still \$state after`).MatchString(body) {
				t.Fatalf("%s must report the deadline failure explicitly", name)
			}
		})
	}
}

func hasUnboundedLoop(body, pattern string) bool {
	idx := -1
	for {
		i := indexOf(body, pattern, idx+1)
		if i < 0 {
			return false
		}
		idx = i
		// The loop header must be guarded by a deadline comparison on
		// the same line.
		line := body[lastNewline(body, i):]
		lineEnd := indexOf(line, "\n", 0)
		if lineEnd >= 0 {
			line = line[:lineEnd]
		}
		if !regexp.MustCompile(`date \+%s`).MatchString(line) {
			return true
		}
	}
}

func indexOf(s, sub string, from int) int {
	if from < 0 || from >= len(s) {
		return -1
	}
	for i := from; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func lastNewline(s string, before int) int {
	for i := before - 1; i >= 0; i-- {
		if s[i] == '\n' {
			return i + 1
		}
	}
	return 0
}
