// Package plugin runs external discovery commands for providers that do not
// implement list resources.
//
// A plugin is any command that prints one JSON object per line:
//
//	{"type":"vault_policy","identity":{"id":"ci"},"display_name":"ci","import_id":"ci"}
//	{"error":"sys/auth: permission denied"}
//	{"done":true}
//
// "type" is the Terraform resource type. "identity" holds the attributes to
// match against state; when state has no resource identity they are compared
// with the instance's attributes (usually {"id": ...} is enough). An "error"
// line reports partial coverage without failing the run. A "done": true
// line confirms that the check finished, even when it found no objects.
// "unverified": true marks objects whose cluster or account state cannot
// confirm: they match only as unverified, unless the user states that the
// states belong to the scanned cluster (-plugin-cluster-states).
package plugin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/proc"
	"github.com/zclconf/go-cty/cty"
)

type line struct {
	model.Resource
	Error string          `json:"error"`
	Done  json.RawMessage `json:"done"`
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

// Run executes command with sh -c and reports whether a done line was seen.
// Warnings are partial failures; the error is returned when the command
// fails or prints invalid output. Reports and errors use name (plugin1,
// plugin2, ...), never the command line: it may carry tokens, and shell
// quoting makes it impossible to cut out safely.
func Run(ctx context.Context, command, name string) ([]model.Resource, []error, bool, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	proc.Group(cmd)
	cmd.Stderr = os.Stderr
	var res []model.Resource
	var warns []error
	var done bool
	var parseErr error
	err := proc.Stream(ctx, cmd, func(out io.Reader) error {
		res, warns, done, parseErr = parseReader(out, name)
		return parseErr
	})
	if ctx.Err() != nil {
		return nil, nil, false, ctx.Err()
	}
	if parseErr != nil {
		return nil, nil, false, parseErr
	}
	if err != nil {
		return nil, nil, false, fmt.Errorf("%s: %w", name, err)
	}
	return res, warns, done, nil
}

// typeName is a Terraform resource type name, providerRef a provider
// configuration reference (vault, vault.other); import blocks are written
// from them.
var (
	typeName    = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)
	providerRef = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9-]*(\.[a-zA-Z_][a-zA-Z0-9_-]*)?$`)
)

// Parse decodes plugin output and reports whether a done line was seen.
func Parse(out []byte, source string) ([]model.Resource, []error, bool, error) {
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

func parseReader(out io.Reader, source string) ([]model.Resource, []error, bool, error) {
	var res []model.Resource
	var warns []error
	var done bool
	sc := bufio.NewScanner(&outputReader{reader: out, left: maxOutputBytes})
	sc.Buffer(make([]byte, min(1<<20, maxLineBytes)), maxLineBytes)
	for n := 1; sc.Scan(); n++ {
		if err := sc.Err(); err != nil {
			return nil, nil, false, fmt.Errorf("%s: %w", source, err)
		}
		b := bytes.TrimSpace(sc.Bytes())
		if len(b) == 0 {
			continue
		}
		var l line
		if err := model.DecodeJSON(b, &l); err != nil {
			return nil, nil, false, fmt.Errorf("%s line %d: %w", source, n, err)
		}
		if l.Done != nil || bytes.ContainsRune(b, '\\') {
			var fields map[string]json.RawMessage
			if err := model.DecodeJSON(b, &fields); err != nil {
				return nil, nil, false, fmt.Errorf("%s line %d: %w", source, n, err)
			}
			for key := range fields {
				if key != "done" && strings.EqualFold(key, "done") {
					return nil, nil, false, fmt.Errorf(`%s line %d: "done" must be lowercase`, source, n)
				}
			}
			if value, ok := fields["done"]; ok {
				if len(fields) != 1 || !bytes.Equal(value, []byte("true")) {
					return nil, nil, false, fmt.Errorf(`%s line %d: "done" must be true and the only key`, source, n)
				}
				done = true
				continue
			}
		}
		if len(res)+len(warns) >= maxResources {
			return nil, nil, false, fmt.Errorf("%s: output exceeds %d resource limit", source, maxResources)
		}
		if l.Error != "" {
			warns = append(warns, fmt.Errorf("%s: %s", source, l.Error))
			continue
		}
		if l.Type == "" || len(l.Identity) == 0 {
			return nil, nil, false, fmt.Errorf("%s line %d: %w", source, n, errors.New(`"type" and "identity" are required`))
		}
		if !typeName.MatchString(l.Type) || l.Provider != "" && !providerRef.MatchString(l.Provider) {
			return nil, nil, false, fmt.Errorf("%s line %d: %w", source, n, errors.New(`"type" or "provider" is not a valid name`))
		}
		for _, k := range model.SortedKeys(l.Identity) {
			// jq gives null for a missing field; that key would then
			// confirm nothing and the rest (an account) would match alone.
			if l.Identity[k] == nil {
				return nil, nil, false, fmt.Errorf("%s line %d: identity key %q is null", source, n, k)
			}
		}
		if num := badNumber(l.Identity); num != "" {
			return nil, nil, false, fmt.Errorf("%s line %d: identity number %.40s is out of range for Terraform", source, n, num)
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
			return nil, nil, false, fmt.Errorf("%s: output line exceeds %d byte limit", source, maxLineBytes)
		}
		return nil, nil, false, fmt.Errorf("%s: %w", source, err)
	}
	return res, warns, done, nil
}

// badNumber returns the first number in v that JSON allows but HCL cannot
// read (1e7000000000): written into an import block, it would make the
// whole file invalid.
func badNumber(v any) string {
	switch t := v.(type) {
	case json.Number:
		if _, err := cty.ParseNumberVal(t.String()); err != nil {
			return t.String()
		}
	case []any:
		for _, e := range t {
			if bad := badNumber(e); bad != "" {
				return bad
			}
		}
	case map[string]any:
		for _, k := range model.SortedKeys(t) {
			if bad := badNumber(t[k]); bad != "" {
				return bad
			}
		}
	}
	return ""
}
