package process

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"syscall"
	"time"
)

type Result struct {
	ExitCode int
	Signaled bool
	Signal   string
}
type Command struct {
	Executable string
	Args       []string
	Dir        string
	Env        []string
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
}
type Running struct {
	PID       int
	PGID      int
	StartedAt time.Time
}
type Runner interface {
	Run(context.Context, Command) (Result, error)
}
type SupervisedRunner interface {
	RunSupervised(context.Context, Command, func(Running) error) (Result, error)
}
type Native struct{}

func (Native) Run(ctx context.Context, command Command) (Result, error) {
	return (Native{}).RunSupervised(ctx, command, nil)
}

func (Native) RunSupervised(ctx context.Context, command Command, onStart func(Running) error) (Result, error) {
	cmd := exec.Command(command.Executable, command.Args...)
	cmd.Dir, cmd.Env = command.Dir, command.Env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = command.Stdin, command.Stdout, command.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return Result{}, err
	}
	running := Running{PID: cmd.Process.Pid, PGID: cmd.Process.Pid, StartedAt: time.Now().UTC()}
	if onStart != nil {
		if err := onStart(running); err != nil {
			terminate(cmd.Process.Pid)
			_, _ = cmd.Process.Wait()
			return Result{}, err
		}
	}
	cancelDone := make(chan struct{})
	var cancelErr error
	go func() {
		select {
		case <-ctx.Done():
			terminate(cmd.Process.Pid)
		case <-cancelDone:
		}
	}()
	err := cmd.Wait()
	close(cancelDone)
	if ctx.Err() != nil {
		cancelErr = ctx.Err()
	}
	result := Result{ExitCode: 0}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
		if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			result.Signaled = true
			result.Signal = status.Signal().String()
		}
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		return result, err
	}
	if cancelErr != nil {
		return result, cancelErr
	}
	return result, nil
}

func terminate(pid int) {
	// The worker owns the child and therefore the group identity. TERM is
	// followed by bounded KILL without holding any run-store lock.
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	timer := time.NewTimer(500 * time.Millisecond)
	<-timer.C
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}
