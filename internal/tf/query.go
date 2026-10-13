package tf

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/proc"
)

// Failure is a list block that reported an error, so its type was not fully
// listed.
type Failure struct {
	Address string `json:"address"`
	Type    string `json:"type"`
	// Scope is where the failure applies ("zone example.com"); empty when
	// it concerns the type in every scope.
	Scope string `json:"scope,omitempty"`
	// Root is the root module whose query failed ("" for -scan).
	Root string `json:"root,omitempty"`
	// Provider is the local name of the provider the block used.
	Provider string `json:"-"`
	// Category groups failures by cause (see Classify); set by Classify.
	Category string `json:"category,omitempty"`
	Message  string `json:"message"`
	// Elsewhere marks a failure read in another place than the listing
	// would have looked: zones read in the provider's project for a query
	// with its own -arg project. A disabled API there proves nothing about
	// the query's project.
	Elsewhere bool `json:"-"`
}

// Failure categories.
const (
	FailMissingArg = "missing argument"
	FailAPIOff     = "API not enabled"
	FailDenied     = "permission denied"
	FailNotFound   = "not found"
	FailTruncated  = "truncated"
	FailCrash      = "crashed"
	FailRetired    = "API retired"
	FailProvider   = "provider bug"
	FailRejected   = "request rejected (feature not on the plan?)"
	FailOther      = "other"
)

// Classify sets the category of f and shortens its message to one line:
// providers return whole API error bodies (a GCP SERVICE_DISABLED error
// carries JSON with activation URLs), which drown the summary.
func (f *Failure) Classify() {
	m := strings.ToLower(f.Message)
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(m, s) {
				return true
			}
		}
		return false
	}
	switch {
	case strings.HasPrefix(m, "crashed") || strings.Contains(m, " crashed: "):
		f.Category = FailCrash
	case has("service_disabled", "has not been used in project", "api has not been enabled", "it is disabled. enable it"):
		f.Category = FailAPIOff
	case has("cannot determine zone", "cannot determine region", "missing required argument", "is required", "must be set"):
		f.Category = FailMissingArg
	case has("permission_denied", "permission denied", "accessdenied", "access denied", "unauthorized", "forbidden", "error 403", "statuscode: 403", "authentication error"):
		f.Category = FailDenied
	case has("its limit", "max_items", "there may be more"):
		f.Category = FailTruncated
	case has("not_found", "not found", "error 404", "statuscode: 404"):
		f.Category = FailNotFound
	case has("410 gone", "api.deprecated"):
		f.Category = FailRetired
	case has("report this issue to the provider developers", "rfc3339 string value is null"):
		f.Category = FailProvider
	case has("400 bad request"):
		f.Category = FailRejected
	default:
		f.Category = FailOther
	}
	if f.Elsewhere && f.Category == FailAPIOff {
		f.Category = FailOther
	}
	f.Message = OneLine(f.Message, 300)
}

// OneLine joins the lines of msg, drops a trailing JSON body and cuts the
// result to max bytes at a rune boundary.
func OneLine(msg string, max int) string {
	msg = strings.Join(strings.Fields(msg), " ")
	// "...: googleapi: Error 403: ..., Details: [ { "@type": ... } ]"
	for _, marker := range []string{" Details: [", " details: [", " {\"", " [{"} {
		if i := strings.Index(msg, marker); i > 0 {
			msg = strings.TrimRight(msg[:i], " ,:;")
		}
	}
	if len(msg) > max {
		cut := max
		for cut > 0 && !utf8.RuneStart(msg[cut]) {
			cut--
		}
		msg = msg[:cut] + "..."
	}
	return msg
}

// Listing is a list block instance and the resource type it lists.
type Listing struct {
	Address string
	Type    string
}

type QueryResult struct {
	Found  []model.Resource
	Failed []Failure
	// Started counts list block instances that started, and generated
	// blocks that failed to read the values of their auto arguments.
	Started  int
	Complete int // list block instances that finished
	// Done are the instances that finished.
	Done []Listing
	// DataErrors are errors of data sources (data.<type>.<name>): the list
	// blocks that need their values do not start.
	DataErrors []Failure
	// Unfinished are the list block instances that started but neither
	// finished nor reported an error of their own.
	Unfinished []string
	errors     int // error diagnostics tied to a list block
}

const (
	tempMarkerPrefix   = "# tforphans temporary file, run "
	tempQuerySuffix    = ".tfquery.hcl"
	tempProviderSuffix = "_override.tf"
	tempAutoSuffix     = "_auto.tf"
	tempVersionSuffix  = "_version_override.tf"
)

var (
	tempSuffixes = []string{tempQuerySuffix, tempProviderSuffix, tempAutoSuffix, tempVersionSuffix}
	tempMarker   = fmt.Sprintf("%s%d-%s\n", tempMarkerPrefix, os.Getpid(), rand.Text())
)

func tempName(suffix string) string {
	return fmt.Sprintf("zz_tforphans_%d%s", os.Getpid(), suffix)
}

func tempPID(name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, "zz_tforphans_")
	if !ok {
		return 0, false
	}
	for _, suffix := range tempSuffixes {
		digits, ok := strings.CutSuffix(rest, suffix)
		if !ok || digits == "" || strings.ContainsFunc(digits, func(r rune) bool { return r < '0' || r > '9' }) {
			continue
		}
		pid, err := strconv.Atoi(digits)
		return pid, err == nil && pid > 0
	}
	return 0, false
}

// writeTemp creates a marked file, refusing to overwrite an existing file.
func writeTemp(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(f, tempMarker); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}

// RemoveStale removes the temporary files of tforphans runs whose process
// no longer exists (killed before it could clean up), and fails when a
// running one still has files in dir.
func RemoveStale(dir string) error {
	stale, _ := filepath.Glob(filepath.Join(dir, "zz_tforphans_*"))
	var busy []string
	for _, path := range stale {
		pid, ok := tempPID(filepath.Base(path))
		if !ok {
			continue
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		prefix := make([]byte, len(tempMarkerPrefix))
		_, err = io.ReadFull(f, prefix)
		f.Close()
		if err != nil || string(prefix) != tempMarkerPrefix {
			continue
		}
		if pid == os.Getpid() || proc.Alive(pid) {
			busy = append(busy, path)
			continue
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	if len(busy) > 0 {
		return fmt.Errorf("%s holds files of another tforphans run that may still be working; wait for it, or remove them: %s", dir, strings.Join(busy, ", "))
	}
	return nil
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
	// labels) or keep changing the configuration. Those of a process that
	// is gone are removed; another run still working here is an error.
	if err := RemoveStale(r.Dir); err != nil {
		return nil, err
	}
	if providers := p.ProvidersHCL(); providers != nil {
		path := filepath.Join(r.Dir, tempName(tempProviderSuffix))
		if err := writeTemp(path, providers); err != nil {
			return nil, err
		}
		defer os.Remove(path)
	}
	if auto := p.AutoHCL(); auto != nil {
		path := filepath.Join(r.Dir, tempName(tempAutoSuffix))
		if err := writeTemp(path, auto); err != nil {
			return nil, err
		}
		defer os.Remove(path)
	}
	path := filepath.Join(r.Dir, tempName(tempQuerySuffix))
	if err := writeTemp(path, p.HCL(limitFor(""))); err != nil {
		return nil, err
	}
	defer os.Remove(path)

	args := []string{"query", "-json", "-no-color"}
	out, stderr, runErr := r.run(ctx, args...)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// Terraform before 1.14 and OpenTofu print this (on stdout).
	if runErr != nil && strings.Contains(string(out)+string(stderr), `no command named "query"`) {
		return nil, fmt.Errorf("%s has no query command: list resources need Terraform 1.14 or newer (OpenTofu has none yet). Upgrade, or pass -no-query and list objects with -scan or -plugin instead", filepath.Base(r.Bin))
	}
	res, fatal, err := ParseQueryStream(out, r.Dir, keys, limitFor)
	if err != nil {
		return nil, err
	}
	// The regions or zones of an auto argument could not be read: its
	// queries fail (a disabled Compute API is classified as such).
	failedAuto := map[string]bool{}
	for _, e := range res.DataErrors {
		deps, ok := p.AutoDependents(e.Address)
		if !ok {
			fatal = append(fatal, e.Address+": "+e.Message)
			continue
		}
		for _, q := range deps {
			if !failedAuto[q.Address()] {
				failedAuto[q.Address()] = true
				res.Failed = append(res.Failed, Failure{Address: q.Address(), Type: q.Type, Message: "reading the values of -arg ...=auto: " + e.Message,
					Elsewhere: slices.ContainsFunc(q.Args, func(a Arg) bool { return a.Name == "project" })})
				res.errors++
				// It got as far as asking the provider: for a disabled
				// API that is the check (a run is not "nothing checked").
				res.Started++
			}
		}
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
		Address        string         `json:"address"`
		DisplayName    string         `json:"display_name"`
		Identity       map[string]any `json:"identity"`
		ResourceType   string         `json:"resource_type"`
		ResourceObject map[string]any `json:"resource_object"`
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
			res.Done = append(res.Done, Listing{Address: c.Address, Type: c.ResourceType})
			block, _, _ := strings.Cut(c.Address, "[")
			switch limit := limitFor(block); {
			case limit <= 0 && c.Total > 0:
				res.Failed = append(res.Failed, Failure{Address: c.Address, Type: c.ResourceType,
					Message: fmt.Sprintf("returned %d results and its limit is not a constant; there may be more", c.Total)})
			case limit > 0 && c.Total >= limit:
				hint := "; raise limit in the list block, or -limit for the generated ones"
				if limit == DefaultListLimit {
					hint = fmt.Sprintf("; %d is Terraform's default when a list block of yours sets no limit: add limit = N to it (generated blocks take -limit)", DefaultListLimit)
				}
				res.Failed = append(res.Failed, Failure{Address: c.Address, Type: c.ResourceType,
					Message: fmt.Sprintf("returned %d results, its limit; there may be more%s", c.Total, hint)})
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
				Attributes:    f.ResourceObject,
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
		if strings.HasPrefix(e.Address, "data.") {
			res.DataErrors = append(res.DataErrors, e)
			continue
		}
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
