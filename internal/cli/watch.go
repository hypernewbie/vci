package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/hypernewbie/vci/internal/model"
)

const (
	defaultWatchInterval = 3 * time.Second
	watchIntervalMin     = 1
	watchIntervalMax     = 3600
)

// runWatch waits for a run to reach a terminal state. Status changes go to
// stderr; stdout receives one final JSON envelope so watch remains composable
// with the rest of the CLI.
func runWatch(args []string, stdout, stderr io.Writer) int {
	id, interval, exitStatus, verr := parseWatchArgs(args)
	if verr != nil {
		return writeFailure(stdout, "watch", verr)
	}

	ctx := context.Background()
	var last model.RunState
	for {
		response, code := checkRun(ctx, []string{string(id)})
		if code != 0 {
			if response.Error != nil {
				return writeFailure(stdout, "watch", response.Error)
			}
			return writeFailure(stdout, "watch", model.NewError("watch_failed", model.FailureInfrastructure, "check returned an invalid response", true))
		}
		state, ok := watchState(response.Data)
		if !ok {
			return writeFailure(stdout, "watch", model.NewError("invalid_response", model.FailureInfrastructure, "run response has no valid state", true))
		}
		if state != last {
			fmt.Fprintf(stderr, "vci: %s %s\n", id, state)
			last = state
		}
		if model.IsTerminal(state) {
			if err := Write(stdout, Success("watch", response.Data)); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			if exitStatus && state != model.RunSucceeded {
				return 1
			}
			return 0
		}
		timer := time.NewTimer(interval)
		<-timer.C
	}
}

// parseWatchArgs parses `vci watch <run-id>` and gh-style watch flags.
func parseWatchArgs(args []string) (model.RunID, time.Duration, bool, *model.VciError) {
	if len(args) == 0 || !model.ValidRunID(model.RunID(args[0])) {
		return "", 0, false, watchUsage()
	}
	id := model.RunID(args[0])
	interval := defaultWatchInterval
	exitStatus := false
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--exit-status":
			exitStatus = true
		case "--interval":
			if i+1 >= len(args) {
				return "", 0, false, model.NewError("invalid_arguments", model.FailureUsage, "--interval requires a value between 1 and 3600 seconds.", false)
			}
			seconds, err := strconv.Atoi(args[i+1])
			if err != nil || seconds < watchIntervalMin || seconds > watchIntervalMax {
				return "", 0, false, model.NewError("invalid_arguments", model.FailureUsage, "--interval must be an integer between 1 and 3600 seconds.", false)
			}
			interval = time.Duration(seconds) * time.Second
			i++
		default:
			return "", 0, false, model.NewError("invalid_arguments", model.FailureUsage, fmt.Sprintf("Unknown watch flag %q.", args[i]), false)
		}
	}
	return id, interval, exitStatus, nil
}

func watchUsage() *model.VciError {
	return model.NewError("invalid_arguments", model.FailureUsage, "Usage: watch <run-id> [--interval <seconds>] [--exit-status].", false)
}

func watchState(data any) (model.RunState, bool) {
	encoded, err := json.Marshal(data)
	if err != nil {
		return "", false
	}
	var value struct {
		State model.RunState `json:"state"`
	}
	if err := json.Unmarshal(encoded, &value); err != nil || !value.State.Valid() {
		return "", false
	}
	return value.State, true
}
