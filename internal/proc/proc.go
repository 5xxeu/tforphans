// Package proc runs child commands so that cancelling them stops
// everything they started.
package proc

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync/atomic"
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

type drainReader struct {
	pipe     *os.File
	delay    time.Duration
	draining atomic.Bool
	expired  atomic.Bool
	timer    *time.Timer
}

func (r *drainReader) Read(p []byte) (int, error) {
	if r.draining.Load() {
		// Bound blocked reads, not time spent consuming output.
		limit := time.Now()
		if !r.expired.Load() {
			limit = limit.Add(r.delay)
		}
		_ = r.pipe.SetReadDeadline(limit)
	}
	n, err := r.pipe.Read(p)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		r.expired.Store(true)
		err = io.EOF
	} else if errors.Is(err, os.ErrClosed) && r.expired.Load() {
		err = io.EOF
	}
	return n, err
}

func (r *drainReader) drain() <-chan time.Time {
	r.draining.Store(true)
	delay := r.delay
	if r.expired.Load() {
		delay = 0
	}
	if err := r.pipe.SetReadDeadline(time.Now().Add(delay)); err != nil {
		// Pipes without deadlines are closed after WaitDelay.
		r.timer = time.NewTimer(delay)
		return r.timer.C
	}
	return nil
}

func (r *drainReader) close() {
	if r.timer != nil {
		r.timer.Stop()
	}
	r.pipe.Close()
}

// Stream runs cmd and consumes stdout without bounding the reading time.
// Group must be called before Stream. Stdin must be nil or an *os.File.
func Stream(ctx context.Context, cmd *exec.Cmd, consume func(io.Reader) error) error {
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	reader := &drainReader{pipe: pr, delay: cmd.WaitDelay}
	defer reader.close()
	defer pw.Close()
	cmd.Stdout = pw
	stderr := cmd.Stderr
	var stderrReader *drainReader
	var stderrWrite *os.File
	if _, ok := stderr.(*os.File); stderr != nil && !ok {
		pr, pw, err := os.Pipe()
		if err != nil {
			return err
		}
		stderrReader = &drainReader{pipe: pr, delay: cmd.WaitDelay}
		defer stderrReader.close()
		stderrWrite = pw
		defer pw.Close()
		cmd.Stderr = pw
	}
	err = cmd.Start()
	pw.Close()
	if stderrWrite != nil {
		stderrWrite.Close()
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	// No os/exec copy goroutines delay Wait after reaping. The only signal
	// race is between reap inside Wait and our receive, as in os/exec itself.
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	read := make(chan error, 1)
	go func() { read <- consume(reader) }()
	var stderrRead chan error
	if stderrReader != nil {
		stderrRead = make(chan error, 1)
		go func() {
			_, err := io.Copy(stderr, stderrReader)
			stderrRead <- err
		}()
	}
	var waitErr, readErr, stderrErr error
	var deadline, stderrDeadline <-chan time.Time
	ctxDone := ctx.Done()
	for wait != nil || read != nil || stderrRead != nil {
		select {
		case waitErr = <-wait:
			wait = nil
			if cmd.WaitDelay > 0 {
				// After reap, bound reads without signalling a reused group ID.
				if read != nil {
					deadline = reader.drain()
				}
				if stderrRead != nil {
					stderrDeadline = stderrReader.drain()
				}
			}
		case readErr = <-read:
			read = nil
			if readErr != nil && wait != nil {
				_ = cmd.Cancel()
			}
			reader.close()
			deadline = nil
		case stderrErr = <-stderrRead:
			stderrRead = nil
			if stderrErr != nil && wait != nil {
				_ = cmd.Cancel()
			}
			stderrReader.close()
			stderrDeadline = nil
		case <-ctxDone:
			if wait != nil {
				_ = cmd.Cancel()
			}
			reader.close()
			if stderrReader != nil {
				stderrReader.close()
			}
			ctxDone = nil
		case <-deadline:
			reader.expired.Store(true)
			reader.close()
			deadline = nil
		case <-stderrDeadline:
			stderrReader.expired.Store(true)
			stderrReader.close()
			stderrDeadline = nil
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if readErr != nil {
		return readErr
	}
	if waitErr != nil {
		return waitErr
	}
	if stderrErr != nil {
		return stderrErr
	}
	if reader.expired.Load() || (stderrReader != nil && stderrReader.expired.Load()) {
		return exec.ErrWaitDelay
	}
	return nil
}
