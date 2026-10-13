// Package proc runs child commands so that cancelling them stops
// everything they started.
package proc

import (
	"bytes"
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

const maxStderrSize = 1 << 20

// StderrBuffer retains the last 1 MiB, discarding earlier output.
type StderrBuffer struct {
	data []byte
}

func (b *StderrBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if n == 0 {
		return 0, nil
	}
	if b.data == nil {
		b.data = make([]byte, 0, maxStderrSize)
	}
	if n >= maxStderrSize {
		b.data = append(b.data[:0], p[n-maxStderrSize:]...)
	} else {
		if excess := len(b.data) + n - maxStderrSize; excess > 0 {
			b.data = b.data[:copy(b.data, b.data[excess:])]
		}
		b.data = append(b.data, p...)
	}
	return n, nil
}

func (b *StderrBuffer) Bytes() []byte { return b.data }

type drainReader struct {
	pipe     *os.File
	delay    time.Duration
	fixed    bool
	draining atomic.Bool
	expired  atomic.Bool
	timer    *time.Timer
}

func (r *drainReader) Read(p []byte) (int, error) {
	if !r.fixed && r.draining.Load() {
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

func (r *drainReader) drain(exited time.Time) <-chan time.Time {
	r.draining.Store(true)
	limit := time.Now().Add(r.delay)
	if r.fixed {
		limit = exited.Add(r.delay)
	}
	if r.expired.Load() {
		limit = time.Now()
	}
	if err := r.pipe.SetReadDeadline(limit); err != nil || r.fixed {
		// Close fixed drains and pipes without deadline support.
		r.timer = time.NewTimer(time.Until(limit))
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
// Stderr writers other than *os.File receive only the last 1 MiB.
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
		stderrReader = &drainReader{pipe: pr, delay: cmd.WaitDelay, fixed: true}
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
	type result struct {
		err    error
		exited time.Time
	}
	wait := make(chan result, 1)
	go func() {
		err := cmd.Wait()
		wait <- result{err: err, exited: time.Now()}
	}()
	read := make(chan error, 1)
	go func() { read <- consume(reader) }()
	var stderrRead chan error
	if stderrReader != nil {
		stderrRead = make(chan error, 1)
		go func() {
			var tail StderrBuffer
			_, err := io.Copy(&tail, stderrReader)
			if _, writeErr := io.Copy(stderr, bytes.NewReader(tail.Bytes())); err == nil {
				err = writeErr
			}
			stderrRead <- err
		}()
	}
	var waitErr, readErr, stderrErr error
	var deadline, stderrDeadline <-chan time.Time
	ctxDone := ctx.Done()
	for wait != nil || read != nil || stderrRead != nil {
		select {
		case result := <-wait:
			waitErr = result.err
			wait = nil
			if cmd.WaitDelay > 0 {
				// After reap, bound reads without signalling a reused group ID.
				if read != nil {
					deadline = reader.drain(result.exited)
				}
				if stderrRead != nil {
					stderrDeadline = stderrReader.drain(result.exited)
				}
			}
		case readErr = <-read:
			read = nil
			reader.close()
			deadline = nil
			if readErr != nil {
				if stderrReader != nil {
					stderrReader.close()
					stderrDeadline = nil
				}
				if wait != nil {
					kill(cmd)
				}
			}
		case stderrErr = <-stderrRead:
			stderrRead = nil
			if stderrErr != nil && wait != nil {
				kill(cmd)
			}
			stderrReader.close()
			stderrDeadline = nil
		case <-ctxDone:
			if wait != nil {
				kill(cmd)
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

// kill signals a command that Wait has not reaped yet. The group signal
// misses a command that moved to another group, so the command itself is
// killed as well.
func kill(cmd *exec.Cmd) {
	_ = cmd.Cancel()
	_ = cmd.Process.Kill()
}
