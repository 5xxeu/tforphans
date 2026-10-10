package tf

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/5xxeu/tforphans/internal/model"
)

// Failure is a list block that reported an error, so its type was not fully
// listed.
type Failure struct {
	Address string
	Type    string
	Message string
}

type QueryResult struct {
	Found    []model.Resource
	Failed   []Failure
	Started  int // list block instances that started
	Complete int // list block instances that finished
	// Unfinished are the list block instances that started but neither
	// finished nor reported an error of their own.
	Unfinished []string
	errors     int // error diagnostics tied to a list block
}

// writeTemp creates path, refusing to overwrite an existing file.
func writeTemp(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}

// Query writes the plan's list blocks (and temporary provider declarations)
// next to the module's configuration, runs
// `terraform query -json` and removes the files again. List blocks the user
// keeps in the module's own .tfquery.hcl files run as well.
//
// A failing list block does not stop the others; it is reported in
// QueryResult.Failed. Errors that abort the whole query are returned.
// limitFor returns the limit of the list block at an address, 0 if unknown.
// keys returns the identity keys of the objects a list block finds.
func (r Runner) Query(ctx context.Context, p Plan, limitFor func(address string) int, keys KeysFunc) (*QueryResult, error) {
	// Files left by a run that was killed would clash with this one (same
	// labels) or keep changing the configuration: say so plainly.
	if stale, _ := filepath.Glob(filepath.Join(r.Dir, "zz_tforphans_*")); len(stale) > 0 {
		return nil, fmt.Errorf("%s holds files of an interrupted tforphans run; remove them: %s", r.Dir, strings.Join(stale, ", "))
	}
	base := filepath.Join(r.Dir, fmt.Sprintf("zz_tforphans_%d", os.Getpid()))
	if providers := p.ProvidersHCL(); providers != nil {
		if err := writeTemp(base+"_override.tf", providers); err != nil {
			return nil, err
		}
		defer os.Remove(base + "_override.tf")
	}
	if err := writeTemp(base+".tfquery.hcl", p.HCL(limitFor(""))); err != nil {
		return nil, err
	}
	defer os.Remove(base + ".tfquery.hcl")

	args := []string{"query", "-json", "-no-color"}
	out, stderr, runErr := r.run(ctx, args...)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	res, fatal, err := ParseQueryStream(out, r.Dir, keys, limitFor)
	if err != nil {
		return nil, err
	}
	if len(fatal) > 0 {
		return nil, fmt.Errorf("terraform query in %s:\n%s", r.Dir, strings.Join(fatal, "\n"))
	}
	if runErr != nil && !res.explained() {
		err := cmdError(args, stderr, runErr)
		if len(res.Unfinished) > 0 {
			err = fmt.Errorf("%w\nlist blocks that did not finish: %s", err, strings.Join(res.Unfinished, ", "))
		}
		return nil, err
	}
	for _, addr := range res.Unfinished {
		res.Failed = append(res.Failed, Failure{Address: addr, Type: listType(addr), Message: "did not finish"})
	}
	return res, nil
}

// explained reports whether a failed run is accounted for by the list
// blocks: some reported their own error and every other one finished. A
// crash (a killed process, a provider panic) after a limit warning must not
// pass for a run with one broken block.
func (res *QueryResult) explained() bool {
	return res.errors > 0 && len(res.Unfinished) == 0
}

// KeysFunc returns the identity keys (see IdentityKeys) of the objects of
// a type found by the list block at an address.
type KeysFunc func(block, resourceType string) (required, attribute []string)

type queryMessage struct {
	Type      string `json:"type"`
	ListStart *struct {
		Address string `json:"address"`
	} `json:"list_start"`
	ListResourceFound *struct {
		Address      string         `json:"address"`
		DisplayName  string         `json:"display_name"`
		Identity     map[string]any `json:"identity"`
		ResourceType string         `json:"resource_type"`
	} `json:"list_resource_found"`
	ListComplete *struct {
		Address      string `json:"address"`
		ResourceType string `json:"resource_type"`
		Total        int    `json:"total"`
	} `json:"list_complete"`
	Diagnostic *struct {
		Severity string `json:"severity"`
		Summary  string `json:"summary"`
		Detail   string `json:"detail"`
		Address  string `json:"address"`
	} `json:"diagnostic"`
}

// ParseQueryStream reads the machine-readable output of `terraform query
// -json`. Error diagnostics tied to a list block become failures; the others
// are returned as fatal, as are any errors when no list block started (the
// configuration was rejected).
//
// A list block that returned as many results as its limit may have stopped
// early, and one whose limit is unknown may have too; both are reported as
// failures so that -strict does not pass on a truncated list.
func ParseQueryStream(data []byte, source string, keys KeysFunc, limitFor func(address string) int) (*QueryResult, []string, error) {
	res := &QueryResult{}
	var errs []Failure
	var started []string
	finished := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var m queryMessage
		if err := model.DecodeJSON(line, &m); err != nil {
			return nil, nil, fmt.Errorf("decode terraform query output: %w", err)
		}
		switch {
		case m.ListStart != nil:
			res.Started++
			started = append(started, m.ListStart.Address)
		case m.ListComplete != nil:
			res.Complete++
			c := m.ListComplete
			finished[c.Address] = true
			block, _, _ := strings.Cut(c.Address, "[")
			switch limit := limitFor(block); {
			case limit <= 0 && c.Total > 0:
				res.Failed = append(res.Failed, Failure{Address: c.Address, Type: c.ResourceType,
					Message: fmt.Sprintf("returned %d results and its limit is not a constant; there may be more", c.Total)})
			case limit > 0 && c.Total >= limit:
				res.Failed = append(res.Failed, Failure{Address: c.Address, Type: c.ResourceType,
					Message: fmt.Sprintf("returned %d results, its limit; there may be more", c.Total)})
			}
		case m.ListResourceFound != nil:
			f := m.ListResourceFound
			block, _, _ := strings.Cut(f.Address, "[")
			required, attribute := keys(block, f.ResourceType)
			res.Found = append(res.Found, model.Resource{
				Type:          f.ResourceType,
				Identity:      f.Identity,
				DisplayName:   f.DisplayName,
				RequiredKeys:  required,
				AttributeKeys: attribute,
				ListAddress:   f.Address,
				Source:        source,
			})
		case m.Diagnostic != nil && m.Diagnostic.Severity == "error":
			d := m.Diagnostic
			msg := d.Summary
			if d.Detail != "" {
				msg += ": " + d.Detail
			}
			errs = append(errs, Failure{Address: d.Address, Type: listType(d.Address), Message: msg})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}
	var fatal []string
	for _, e := range errs {
		if e.Type != "" && res.Started > 0 {
			res.Failed = append(res.Failed, e)
			res.errors++
			continue
		}
		if e.Address != "" {
			fatal = append(fatal, e.Address+": "+e.Message)
		} else {
			fatal = append(fatal, e.Message)
		}
	}
	// An error on a block (list.t.b) accounts for all its instances
	// (list.t.b["k"]), an error on an instance for that instance only.
	for _, addr := range started {
		if finished[addr] {
			continue
		}
		block, _, _ := strings.Cut(addr, "[")
		if !slices.ContainsFunc(res.Failed, func(f Failure) bool { return f.Address == addr || f.Address == block }) {
			res.Unfinished = append(res.Unfinished, addr)
		}
	}
	return res, fatal, nil
}

// listType extracts the resource type from a list block address such as
// list.aws_instance.tfo_aws_instance["region=eu-north-1"].
func listType(addr string) string {
	rest, ok := strings.CutPrefix(addr, "list.")
	if !ok {
		return ""
	}
	typ, _, _ := strings.Cut(rest, ".")
	return typ
}
