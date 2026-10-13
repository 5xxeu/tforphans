// Package plugin runs external discovery commands for providers that do not
// implement list resources.
//
// A plugin is any command that prints one JSON object per line:
//
//	{"type":"vault_policy","identity":{"id":"ci"},"display_name":"ci","import_id":"ci"}
//	{"error":"sys/auth: permission denied"}
//
// "type" is the Terraform resource type. "identity" holds the attributes to
// match against state; when state has no resource identity they are compared
// with the instance's attributes (usually {"id": ...} is enough). An "error"
// line reports partial coverage without failing the run. "unverified": true
// marks objects whose cluster or account state cannot confirm: they match
// only as unverified, unless the user states that the states belong to the
// scanned cluster (-plugin-cluster-states).
package plugin

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"

	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/proc"
)

type line struct {
	model.Resource
	Error string `json:"error"`
	// Unverified says that state cannot confirm the object's cluster or
	// account (Vault state does not record the Vault address).
	Unverified bool `json:"unverified"`
}

const (
	defaultMaxLineBytes   = 64 << 20
	defaultMaxOutputBytes = 1 << 30
	defaultMaxResources   = 1_000_000
)

var (
	maxLineBytes         = defaultMaxLineBytes
	maxOutputBytes int64 = defaultMaxOutputBytes
	maxResources         = defaultMaxResources
)

// Run executes command with sh -c. Warnings are partial failures; the error
// is returned when the command fails or prints invalid output. Reports and
// errors use name (plugin1, plugin2, ...), never the command line: it may
// carry tokens, and shell quoting makes it impossible to cut out safely.
func Run(ctx context.Context, command, name string) ([]model.Resource, []error, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "sh", "-c", command)
	proc.Group(cmd)
	cmd.Stderr = os.Stderr
	out, stdout := io.Pipe()
	defer out.Close()
	defer stdout.Close()
	// Let exec copy stdout so WaitDelay bounds inherited open pipes.
	cmd.Stdout = stdout
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, fmt.Errorf("%s: %w", name, err)
	}
	wait := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		// Parsing must see EOF even when the command failed.
		stdout.Close()
		wait <- err
	}()
	res, warns, err := parseReader(out, name)
	if err != nil {
		cancel()
		out.CloseWithError(err)
	}
	runErr := <-wait
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	if err != nil {
		return nil, nil, err
	}
	if runErr != nil {
		return nil, nil, fmt.Errorf("%s: %w", name, runErr)
	}
	return res, warns, nil
}

// typeName is a Terraform resource type name, providerRef a provider
// configuration reference (vault, vault.other); import blocks are written
// from them.
var (
	typeName    = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)
	providerRef = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9-]*(\.[a-zA-Z_][a-zA-Z0-9_-]*)?$`)
)

// Parse decodes plugin output.
func Parse(out []byte, source string) ([]model.Resource, []error, error) {
	return parseReader(bytes.NewReader(out), source)
}

type outputReader struct {
	reader io.Reader
	left   int64
}

func (r *outputReader) Read(p []byte) (int, error) {
	if int64(len(p)) > r.left+1 {
		p = p[:r.left+1]
	}
	n, err := r.reader.Read(p)
	if int64(n) > r.left {
		return 0, fmt.Errorf("output exceeds %d byte limit", maxOutputBytes)
	}
	r.left -= int64(n)
	return n, err
}

func parseReader(out io.Reader, source string) ([]model.Resource, []error, error) {
	var res []model.Resource
	var warns []error
	sc := bufio.NewScanner(&outputReader{reader: out, left: maxOutputBytes})
	sc.Buffer(make([]byte, min(1<<20, maxLineBytes)), maxLineBytes)
	for n := 1; sc.Scan(); n++ {
		if err := sc.Err(); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", source, err)
		}
		b := bytes.TrimSpace(sc.Bytes())
		if len(b) == 0 {
			continue
		}
		var l line
		if err := model.DecodeJSON(b, &l); err != nil {
			return nil, nil, fmt.Errorf("%s line %d: %w", source, n, err)
		}
		if l.Error != "" {
			warns = append(warns, fmt.Errorf("%s: %s", source, l.Error))
			continue
		}
		if l.Type == "" || len(l.Identity) == 0 {
			return nil, nil, fmt.Errorf("%s line %d: %w", source, n, errors.New(`"type" and "identity" are required`))
		}
		if !typeName.MatchString(l.Type) || l.Provider != "" && !providerRef.MatchString(l.Provider) {
			return nil, nil, fmt.Errorf("%s line %d: %w", source, n, errors.New(`"type" or "provider" is not a valid name`))
		}
		if len(res) >= maxResources {
			return nil, nil, fmt.Errorf("%s: output exceeds %d resource limit", source, maxResources)
		}
		l.Resource.Source = source
		if l.Unverified {
			l.Resource.Scope = &model.Scope{Unverifiable: true}
		}
		// The protocol: every key given must be present and equal in state,
		// attributes included (built-in sources tolerate a missing account
		// key there, plugins do not).
		l.Resource.RequiredKeys = model.SortedKeys(l.Identity)
		l.Resource.AttributeKeys = nil
		res = append(res, l.Resource)
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, nil, fmt.Errorf("%s: output line exceeds %d byte limit", source, maxLineBytes)
		}
		return nil, nil, fmt.Errorf("%s: %w", source, err)
	}
	return res, warns, nil
}
