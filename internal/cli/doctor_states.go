package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/5xxeu/tforphans/internal/report"
	"github.com/5xxeu/tforphans/internal/state"
	"github.com/5xxeu/tforphans/internal/tf"
)

func (d *doctorReport) states(ctx context.Context, o *options, roots []string) []state.Snapshot {
	type read struct {
		snapshots []state.Snapshot
		n, total  int
		err       error
	}
	// An s3:// prefix stands for the states under it; one that cannot be
	// listed is a failed source of its own.
	// doctor has no -allow-http.
	s3 := &state.S3{}
	var sources []string
	var listFailed []read
	var listRoots []string
	for _, src := range o.src.states {
		if !state.IsS3Prefix(src) {
			sources = append(sources, src)
			continue
		}
		srcs, _, err := listS3(ctx, o, s3, src)
		if err != nil {
			listFailed = append(listFailed, read{total: 1, err: err})
			listRoots = append(listRoots, state.Redact(src))
			continue
		}
		sources = append(sources, srcs...)
	}
	results := make([]read, len(roots)+len(sources))
	var wg sync.WaitGroup
	sem := make(chan struct{}, o.src.stateParallelism)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			v, err := withSource(ctx, o, func(ctx context.Context) (read, error) {
				v := read{total: 1}
				if i < len(roots) {
					root := roots[i]
					if slices.Contains(o.src.roots, root) {
						v.snapshots, v.n, v.err = pullRootCount(ctx, o, root, &v.total)
					} else {
						v.snapshots, v.n, v.err = pullStateRootCount(ctx, o, root, &v.total)
					}
					return v, v.err
				}
				src := sources[i-len(roots)]
				var raw []byte
				var err error
				if _, _, ok, _ := state.S3Source(src); ok {
					raw, err = s3.Get(ctx, src, o.src.maxStateSize)
				} else {
					raw, err = state.Fetch(ctx, src, false, o.src.maxStateSize)
				}
				if err != nil {
					return v, err
				}
				snapshot, err := state.ParseSnapshot(raw, state.Redact(src))
				if err == nil {
					v.n = 1
					v.snapshots = []state.Snapshot{snapshot}
				}
				return v, err
			})
			v.err = err
			results[i] = v
		}()
	}
	wg.Wait()
	results = append(results, listFailed...)
	var all []state.Snapshot
	n, total := 0, 0
	for i, v := range results {
		root := ""
		if i < len(roots) {
			root = roots[i]
		} else if j := i - len(roots); j < len(sources) {
			root = state.Redact(sources[j])
		} else {
			root = listRoots[j-len(sources)]
		}
		n += v.n
		total += v.total
		all = append(all, v.snapshots...)
		if v.err != nil {
			unread := report.NewUnreadState(root, v.err)
			d.add("states.read", "failed", unread.Error, unread.Root, doctorStateRemediation(root, v.err, o))
			if doctorResponseError(v.err) {
				d.incomplete = true
			}
		}
	}
	status := "ok"
	for _, v := range results {
		if v.err != nil {
			status = "failed"
		}
	}
	d.add("states.read", status, fmt.Sprintf("%d of %d states read", n, total), "", "")
	d.Checks[len(d.Checks)-1].Read, d.Checks[len(d.Checks)-1].Total = &n, &total
	return all
}

func doctorStateRemediation(root string, err error, o *options) string {
	var size *state.TooLargeError
	if errors.As(err, &size) {
		return "Increase -max-state-size above " + fmt.Sprint(size.Max) + " bytes"
	}
	var timeout sourceTimeout
	if errors.As(err, &timeout) || errors.Is(err, context.DeadlineExceeded) {
		return "Increase -source-timeout or check backend connectivity"
	}
	message := strings.ToLower(err.Error())
	for _, text := range []string{"module source has changed", "backend initialization required", "required plugins are not installed", "please run terraform init", "run terraform init", "not initialized", "initialization required", "initialization is required", "module not installed", `run "terraform init"`, "run 'terraform init'"} {
		if strings.Contains(message, text) {
			if o.src.terragrunt {
				if kind, e := tf.TerragruntKind(root); e == nil && kind != tf.TerragruntNone {
					return "cd " + shellQuote(root) + " && terragrunt run -- init"
				}
			}
			return "terraform -chdir=" + shellQuote(root) + " init"
		}
	}
	for _, text := range []string{"403", "401", "accessdenied", "access denied", "unauthorized", "forbidden"} {
		if strings.Contains(message, text) {
			return "Check backend credentials and read permissions for " + root
		}
	}
	return "Check backend credentials, state source and Terraform initialization"
}
