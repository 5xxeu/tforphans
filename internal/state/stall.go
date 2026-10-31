package state

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// stallTimeout is how long a request may go without data from the server,
// before the response or in its body. A large state on a slow link may
// take longer than that as a whole; a server that stops sending must not
// hang the run (-timeout is off by default). A variable for tests.
var stallTimeout = 2 * time.Minute

var errStalled = errors.New("no data from the server")

// stallTransport cancels a request once its server sends nothing for
// stallTimeout.
type stallTransport struct{ base http.RoundTripper }

func (t stallTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	d := stallTimeout
	ctx, cancel := context.WithCancelCause(req.Context())
	timer := time.AfterFunc(d, func() { cancel(fmt.Errorf("%w for %s", errStalled, d)) })
	resp, err := t.base.RoundTrip(req.WithContext(ctx))
	if err != nil {
		timer.Stop()
		if cause := context.Cause(ctx); errors.Is(cause, errStalled) {
			err = cause
		}
		cancel(nil)
		return nil, err
	}
	timer.Reset(d)
	resp.Body = &stallBody{ReadCloser: resp.Body, ctx: ctx, cancel: cancel, timer: timer, d: d}
	return resp, nil
}

type stallBody struct {
	io.ReadCloser
	ctx    context.Context
	cancel context.CancelCauseFunc
	timer  *time.Timer
	d      time.Duration
}

func (b *stallBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.timer.Reset(b.d)
	}
	if err != nil && err != io.EOF {
		if cause := context.Cause(b.ctx); errors.Is(cause, errStalled) {
			err = cause
		}
	}
	return n, err
}

func (b *stallBody) Close() error {
	b.timer.Stop()
	err := b.ReadCloser.Close()
	b.cancel(nil)
	return err
}
