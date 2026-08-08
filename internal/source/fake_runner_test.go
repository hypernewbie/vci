package source

import (
	"context"
	"fmt"

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

// matchAll matches every argv. It must be paired with a more specific
// pattern that runs first (the runner iterates patterns in order).
func matchAll() func([]string) bool { return func(_ []string) bool { return true } }

// cancelingRunner returns context.Canceled on every Run call. The
// hosted path propagates this as a wrapped unavailable error.
type cancelingRunner struct{}

func (cancelingRunner) Run(ctx context.Context, cmd process.Command) (process.Result, error) {
	<-ctx.Done()
	return process.Result{}, ctx.Err()
}
