package model

import "testing"

func TestRunTransitions(t *testing.T) {
	legal := [][2]RunState{
		{RunQueued, RunStaging}, {RunStaging, RunRunning},
		{RunRunning, RunCommitting}, {RunCommitting, RunSucceeded},
		{RunRunning, RunFailed}, {RunStaging, RunLost}, {RunLost, RunAborted},
		{RunQueued, RunAborted}, {RunRunning, RunAborted},
	}
	for _, pair := range legal {
		if err := Transition(pair[0], pair[1]); err != nil {
			t.Errorf("expected %s -> %s: %v", pair[0], pair[1], err)
		}
	}
	illegal := [][2]RunState{{RunSucceeded, RunSucceeded}, {RunFailed, RunFailed}, {RunAborted, RunAborted}, {RunLost, RunQueued}, {RunSucceeded, RunRunning}, {RunFailed, RunQueued}, {RunQueued, RunSucceeded}}
	for _, pair := range illegal {
		if err := Transition(pair[0], pair[1]); err == nil {
			t.Errorf("expected %s -> %s to fail", pair[0], pair[1])
		}
	}
}

func TestStatusValues(t *testing.T) {
	for _, state := range []RunState{RunQueued, RunStaging, RunRunning, RunCommitting, RunSucceeded, RunFailed, RunLost, RunAborted} {
		if !state.Valid() {
			t.Errorf("invalid state %q", state)
		}
	}
}
