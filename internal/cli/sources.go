package cli

import (
	"cmp"
	"context"
	"fmt"
	"time"

	"github.com/5xxeu/tforphans/internal/dsource"
	"github.com/5xxeu/tforphans/internal/tf"
)

// sources are providers without list resources that are listed through
// their plural data sources. Credentials come from the provider's
// environment variables, since the generated root has no provider block.
var sources = map[string]dsource.Adapter{
	"cloudflare": dsource.Cloudflare,
	"proxmox":    dsource.Proxmox,
}

// withSource runs one root, scan or plugin under -source-timeout and says
// so when that ran out.
func withSource[T any](ctx context.Context, o *options, f func(context.Context) (T, error)) (T, error) {
	if o.sourceTimeout <= 0 {
		return f(ctx)
	}
	// Waiting for the turn to run terraform init does not count.
	srcCtx, cancel := tf.WithBudget(ctx, o.sourceTimeout)
	defer cancel()
	v, err := f(srcCtx)
	// Also when f returned no error: work after its last command may not
	// look at the context, and a result past the deadline is not complete.
	if ctx.Err() == nil && srcCtx.Err() != nil {
		err = sourceTimeout{o.sourceTimeout, cmp.Or(err, srcCtx.Err())}
	}
	return v, err
}

// sourceTimeout keeps the error it replaces in the chain: a root whose
// query timed out after its state was read is still a queryError.
type sourceTimeout struct {
	after time.Duration
	err   error
}

func (e sourceTimeout) Error() string {
	return fmt.Sprintf("gave up after %s (-source-timeout)", e.after)
}

func (e sourceTimeout) Unwrap() error { return e.err }
