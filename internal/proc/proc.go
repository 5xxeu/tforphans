// Package proc runs child commands so that cancelling them stops
// everything they started.
package proc

import (
	"context"
	"io"
	"os"
	"os/exec"
	"time"
)

// Group makes cancellation of cmd stop the processes it started too (a
// shell pipeline, a terraform wrapper and its terraform), and bounds the
// wait for children that still hold stdout or stderr open after cmd has
// exited or been cancelled.
func Group(cmd *exec.Cmd) {
	ownGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
}

// Stream runs cmd and consumes stdout without bounding the reading time.
// Group must be called before Stream.
func Stream(ctx context.Context, cmd *exec.Cmd, consume func(io.Reader) error) error {
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	defer pr.Close()
	cmd.Stdout = pw
	err = cmd.Start()
	pw.Close()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	read := make(chan error, 1)
	go func() { read <- consume(pr) }()
	var waitErr, readErr error
	var timer *time.Timer
	var deadline <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	ctxDone := ctx.Done()
	for wait != nil || read != nil {
		select {
		case waitErr = <-wait:
			wait = nil
			if waitErr == exec.ErrWaitDelay {
				// WaitDelay already expired on stderr.
				_ = cmd.Cancel()
			}
			if read != nil && cmd.WaitDelay > 0 {
				timer = time.NewTimer(cmd.WaitDelay)
				deadline = timer.C
			}
		case readErr = <-read:
			read = nil
			if readErr != nil {
				_ = cmd.Cancel()
			}
			pr.Close()
			if timer != nil {
				timer.Stop()
				deadline = nil
			}
		case <-ctxDone:
			_ = cmd.Cancel()
			pr.Close()
			ctxDone = nil
		case <-deadline:
			// Kill descendants, but let the reader drain buffered output.
			if err := cmd.Cancel(); err == nil && waitErr == nil {
				waitErr = exec.ErrWaitDelay
			}
			deadline = nil
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if readErr != nil {
		return readErr
	}
	return waitErr
}
