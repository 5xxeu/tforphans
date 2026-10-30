package cli

import (
	"context"
	"fmt"
	"sync"

	"github.com/5xxeu/tforphans/internal/report"
	"github.com/5xxeu/tforphans/internal/state"
)

// fetchState reads one -state source, an S3 object through the run's
// shared client.
func fetchState(ctx context.Context, o *options, s3 *state.S3, src string) ([]byte, error) {
	if _, _, ok, _ := state.S3Source(src); ok {
		return s3.Get(ctx, src, o.src.maxStateSize)
	}
	return state.Fetch(ctx, src, o.src.allowHTTP, o.src.maxStateSize)
}

// listS3 lists the states under an s3:// prefix, with a note about the
// objects it passed over. A prefix with no state is an error: it is more
// likely a typo than an empty environment.
func listS3(ctx context.Context, o *options, s3 *state.S3, prefix string) ([]string, string, error) {
	type listing struct {
		states []string
		other  int
	}
	l, err := withSource(ctx, o, func(ctx context.Context) (listing, error) {
		states, other, err := s3.List(ctx, prefix, o.src.maxRoots)
		return listing{states, other}, err
	})
	if err != nil {
		return nil, "", err
	}
	if len(l.states) == 0 {
		return nil, "", fmt.Errorf("no object ending in .tfstate under %s", state.Redact(prefix))
	}
	note := ""
	if l.other > 0 {
		note = fmt.Sprintf("%s: %d objects without the .tfstate suffix were not read; give a state stored under another key as -state s3://BUCKET/KEY", state.Redact(prefix), l.other)
	}
	return l.states, note, nil
}

// readS3Prefix adds every state under an s3:// prefix to idx. Listing must
// succeed; an object that cannot be read is an unread state, like a root
// of -state-roots, and the others still count.
func readS3Prefix(ctx context.Context, o *options, s3 *state.S3, prefix string, idx *state.Index, sum *report.Summary) error {
	srcs, note, err := listS3(ctx, o, s3, prefix)
	if err != nil {
		return fmt.Errorf("state %s: %w", state.Redact(prefix), err)
	}
	if note != "" {
		o.run.stateNotes = append(o.run.stateNotes, note)
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, o.src.stateParallelism)
	done := 0
	for _, src := range srcs {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			snapshot, err := withSource(ctx, o, func(ctx context.Context) (state.Snapshot, error) {
				data, err := s3.Get(ctx, src, o.src.maxStateSize)
				if err != nil {
					return state.Snapshot{}, err
				}
				return state.ParseSnapshot(data, src)
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				sum.UnreadStates = append(sum.UnreadStates, report.NewUnreadState(src, err))
			} else {
				idx.AddSnapshots([]state.Snapshot{snapshot})
				sum.States++
			}
			done++
			o.stepf("read state %s (%d/%d)", src, done, len(srcs))
		}()
	}
	wg.Wait()
	return ctx.Err()
}
