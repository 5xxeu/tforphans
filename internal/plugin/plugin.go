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

// Run executes command with sh -c. Warnings are partial failures; the error
// is returned when the command fails or prints invalid output. Reports and
// errors use name (plugin1, plugin2, ...), never the command line: it may
// carry tokens, and shell quoting makes it impossible to cut out safely.
func Run(ctx context.Context, command, name string) ([]model.Resource, []error, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	proc.Group(cmd)
	cmd.Stderr = os.Stderr
	out, runErr := cmd.Output()
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	res, warns, err := Parse(out, name)
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
	var res []model.Resource
	var warns []error
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for n := 1; sc.Scan(); n++ {
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
	return res, warns, sc.Err()
}
