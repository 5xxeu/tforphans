// Package state reads Terraform state and answers whether a live resource is
// already managed by it.
package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/5xxeu/tforphans/internal/model"
)

// Instance is one managed resource instance from a state file.
type Instance struct {
	Type    string
	Address string
	// Provider is the provider address the instance belongs to, "" when
	// state does not tell (legacy provider.aws references).
	Provider    string
	Identity    map[string]any
	Attributes  map[string]any
	IDSensitive bool
	// Source is where the instance was read (root and workspace, -state),
	// for -explain.
	Source string
}

type rawState struct {
	Version   int    `json:"version"`
	Lineage   string `json:"lineage"`
	Serial    *int64 `json:"serial"`
	Resources []struct {
		Module    string `json:"module"`
		Mode      string `json:"mode"`
		Type      string `json:"type"`
		Name      string `json:"name"`
		Provider  string `json:"provider"`
		Instances []struct {
			IndexKey            any            `json:"index_key"`
			Identity            map[string]any `json:"identity"`
			Attributes          map[string]any `json:"attributes"`
			SensitiveAttributes any            `json:"sensitive_attributes"`
		} `json:"instances"`
	} `json:"resources"`
}

// Parse decodes a raw state file (format version 4, as written by Terraform
// and returned by `terraform state pull`). Data sources are skipped. Empty
// input is an empty state.
func Parse(data []byte) ([]Instance, error) {
	s, err := ParseSnapshot(data, "")
	return s.Instances, err
}

// ParseSnapshot reads a raw state and retains its provenance.
func ParseSnapshot(data []byte, source string) (Snapshot, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return Snapshot{Sources: []string{source}}, nil
	}
	var st rawState
	if err := model.DecodeJSON(data, &st); err != nil {
		return Snapshot{}, fmt.Errorf("decode state: %w", &model.ResponseError{Err: err})
	}
	if st.Version != 4 {
		return Snapshot{}, &model.ResponseError{Err: fmt.Errorf("unsupported state format version %d (want 4; pass raw state, not `terraform show -json`)", st.Version)}
	}
	var out []Instance
	for _, r := range st.Resources {
		if r.Mode != "managed" {
			continue
		}
		addr := r.Type + "." + r.Name
		if r.Module != "" {
			addr = r.Module + "." + addr
		}
		for _, in := range r.Instances {
			a := addr
			switch k := in.IndexKey.(type) {
			case nil:
			case string:
				a += fmt.Sprintf("[%q]", k)
			default:
				a += fmt.Sprintf("[%v]", k)
			}
			out = append(out, Instance{Type: r.Type, Address: a, Provider: providerAddress(r.Provider), Identity: in.Identity, Attributes: in.Attributes, IDSensitive: sensitiveID(in.SensitiveAttributes)})
		}
	}
	return Snapshot{Lineage: st.Lineage, Serial: st.Serial, Instances: out, Sources: []string{source}}, nil
}

// sensitiveID accepts attribute paths and cty get_attr/index paths. Unknown
// path encodings must not allow a potentially sensitive ID into reports.
func sensitiveID(value any) bool {
	if value == nil {
		return false
	}
	paths, ok := value.([]any)
	if !ok {
		return true
	}
	for _, value := range paths {
		path, ok := value.([]any)
		if !ok || len(path) == 0 {
			return true
		}
		attribute := ""
		for i, value := range path {
			switch step := value.(type) {
			case string:
				if i == 0 {
					attribute = step
				}
			case map[string]any:
				switch step["type"] {
				case "get_attr":
					name, ok := step["value"].(string)
					if !ok || name == "" {
						return true
					}
					if i == 0 {
						attribute = name
					}
				case "index":
					if i == 0 {
						return true
					}
					switch step["value"].(type) {
					case string, json.Number:
					default:
						return true
					}
				default:
					return true
				}
			case json.Number:
				if i == 0 {
					return true
				}
			default:
				return true
			}
		}
		if attribute == "" || attribute == "id" {
			return true
		}
	}
	return false
}

// sensitiveValue reports whether a sensitive_values entry marks any part of
// the value. Unknown encodings count as sensitive.
func sensitiveValue(v any) bool {
	switch v := v.(type) {
	case nil:
		return false
	case bool:
		return v
	case []any:
		return slices.ContainsFunc(v, sensitiveValue)
	case map[string]any:
		for _, x := range v {
			if sensitiveValue(x) {
				return true
			}
		}
		return false
	default:
		return true
	}
}

// providerAddress extracts the provider address from a state reference
// such as module.app.provider["registry.terraform.io/hashicorp/aws"].west.
// Legacy references (provider.aws) and the legacy "-" namespace do not name
// a provider source and give "".
func providerAddress(ref string) string {
	_, rest, ok := strings.Cut(ref, `provider["`)
	if !ok {
		return ""
	}
	addr, _, ok := strings.Cut(rest, `"]`)
	if !ok || strings.Contains(addr, "/-/") {
		return ""
	}
	return addr
}

const DefaultMaxSize int64 = 1 << 30

// TooLargeError is ReadLimited's error for input above the limit. Readers
// of something other than state (a -baseline report) word it themselves:
// -max-state-size does not apply to them.
type TooLargeError struct{ Max int64 }

func (e *TooLargeError) Error() string {
	return fmt.Sprintf("state is larger than %d bytes; raise -max-state-size", e.Max)
}

// ReadLimited reads at most maxSize+1 bytes, rejecting oversized state.
func ReadLimited(r io.Reader, maxSize int64) ([]byte, error) {
	if maxSize <= 0 || maxSize == 1<<63-1 {
		return nil, errors.New("invalid -max-state-size: want a positive size below 9223372036854775807 bytes")
	}
	data, err := io.ReadAll(io.LimitReader(r, maxSize+1))
	if int64(len(data)) > maxSize {
		return nil, &TooLargeError{maxSize}
	}
	return data, err
}

// Fetch reads a state source: "-" for stdin, an https URL (for example a
// GitLab Terraform state address) or a local file. HTTP requests use
// TF_HTTP_USERNAME and TF_HTTP_PASSWORD like Terraform's http backend, and
// only over https. Plain http needs allowHTTP and never carries credentials.
func Fetch(ctx context.Context, src string, allowHTTP bool, maxSize int64) ([]byte, error) {
	scheme := strings.ToLower(urlScheme(src))
	switch {
	case src == "-":
		return readContext(ctx, func() (io.ReadCloser, error) { return io.NopCloser(os.Stdin), nil }, maxSize)
	case scheme == "https", scheme == "http":
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
		if err != nil {
			// The parse error quotes the URL, which may hold a password.
			return nil, fmt.Errorf("invalid state URL %s", Redact(src))
		}
		// Credentials come from the URL's user info (Go would send it as
		// Basic auth on its own) or from TF_HTTP_USERNAME/TF_HTTP_PASSWORD;
		// either way they are sent explicitly and only over https.
		if req.URL.User != nil {
			pass, _ := req.URL.User.Password()
			req.SetBasicAuth(req.URL.User.Username(), pass)
			req.URL.User = nil
		} else if u := os.Getenv("TF_HTTP_USERNAME"); u != "" {
			req.SetBasicAuth(u, os.Getenv("TF_HTTP_PASSWORD"))
		}
		if req.URL.Scheme != "https" {
			if req.Header.Get("Authorization") != "" {
				return nil, fmt.Errorf("refusing to send credentials over %s; use https", req.URL.Scheme)
			}
			if !allowHTTP {
				return nil, errors.New("refusing to read state over plain http, where it can be read and altered in transit; use https, or -allow-http")
			}
		}
		resp, err := httpClient().Do(req)
		if err != nil {
			var ue *url.Error
			if errors.As(err, &ue) {
				// The cause can quote another URL (a bad Location header).
				return nil, fmt.Errorf("%s %s: %s", ue.Op, Redact(ue.URL), redactURLs(ue.Err.Error()))
			}
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: %s", Redact(src), resp.Status)
		}
		data, err := ReadLimited(resp.Body, maxSize)
		if err != nil {
			return nil, fmt.Errorf("GET %s: %w", Redact(src), err)
		}
		return data, nil
	case scheme != "":
		// Any other scheme://: reading it as a file would quote the
		// whole string, credentials included, in the error.
		return nil, fmt.Errorf("unsupported state URL %s: use https, a file or -", Redact(src))
	default:
		return ReadFile(ctx, src, maxSize)
	}
}

// ReadFile is ReadLimited of a file that gives up when ctx ends
// (-timeout): a named pipe nobody writes to blocks already when it is
// opened.
func ReadFile(ctx context.Context, path string, maxSize int64) ([]byte, error) {
	return readContext(ctx, func() (io.ReadCloser, error) { return os.Open(path) }, maxSize)
}

// readContext is ReadLimited of what open returns, giving up when ctx
// ends. The reading goroutine stays blocked until the process exits;
// nothing waits for it.
func readContext(ctx context.Context, open func() (io.ReadCloser, error), maxSize int64) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		r, err := open()
		if err != nil {
			done <- result{nil, err}
			return
		}
		defer r.Close()
		data, err := ReadLimited(r, maxSize)
		done <- result{data, err}
	}()
	select {
	case res := <-done:
		return res.data, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// urlScheme returns the scheme of a "scheme://..." source, in its own
// letter case (schemes are case-insensitive), or "" for anything else.
func urlScheme(src string) string {
	scheme, _, ok := strings.Cut(src, "://")
	if !ok || scheme == "" || !unicode.IsLetter(rune(scheme[0])) {
		return ""
	}
	for _, r := range scheme {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '+' || r == '.' || r == '-') {
			return ""
		}
	}
	return scheme
}

// Redact removes credentials from a state source before it is printed: the
// user info and the query (presigned URLs carry signatures there).
func Redact(src string) string {
	u, err := url.Parse(src)
	if err != nil {
		if strings.Contains(src, "://") {
			return "<invalid URL>"
		}
		return src
	}
	if u.Scheme == "" && u.User == nil && u.RawQuery == "" {
		return src
	}
	u.User = nil
	if u.RawQuery != "" {
		u.RawQuery = "REDACTED"
	}
	u.Fragment = ""
	return u.String()
}

var urlPattern = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s"'<>]+`)

// redactURLs applies Redact to every URL inside a message.
func redactURLs(msg string) string {
	return urlPattern.ReplaceAllStringFunc(msg, Redact)
}

// transport is replaced in tests.
var transport http.RoundTripper = http.DefaultTransport

// httpClient times out instead of hanging on a stalled backend.
//
// On redirects it never sends a Referer (Go would send the previous URL,
// query and all, which for a presigned URL is the signature), never goes
// from https to http (the state could be read or altered in transit, for
// example to add fake managed resources), and sends the Authorization
// header only back to the original host: Go itself keeps it on same-host
// redirects and on redirects to subdomains. Plain http is followed only when
// the request already was plain http, which needs -allow-http.
func httpClient() *http.Client {
	return &http.Client{
		Transport: transport,
		Timeout:   2 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			req.Header.Del("Referer")
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if req.URL.Scheme != "https" && via[0].URL.Scheme == "https" {
				return fmt.Errorf("refusing to follow a redirect from https to %s://%s", req.URL.Scheme, req.URL.Host)
			}
			if req.URL.Host != via[0].URL.Host {
				req.Header.Del("Authorization")
			}
			return nil
		},
	}
}

// ParseShow parses the prior state of `terraform show -json PLAN` (see
// tf.Runner.RefreshedState) into instances.
func ParseShow(data []byte) ([]Instance, error) {
	var plan struct {
		PriorState struct {
			Values struct {
				RootModule showModule `json:"root_module"`
			} `json:"values"`
		} `json:"prior_state"`
	}
	if err := model.DecodeJSON(data, &plan); err != nil {
		return nil, fmt.Errorf("decode plan: %w", err)
	}
	var out []Instance
	var walk func(m showModule)
	walk = func(m showModule) {
		for _, r := range m.Resources {
			if r.Mode == "managed" {
				out = append(out, Instance{Type: r.Type, Address: r.Address, Provider: r.ProviderName, Identity: r.Identity, Attributes: r.Values, IDSensitive: sensitiveValue(r.SensitiveValues["id"])})
			}
		}
		for _, c := range m.ChildModules {
			walk(c)
		}
	}
	walk(plan.PriorState.Values.RootModule)
	return out, nil
}

type showModule struct {
	Resources []struct {
		Address         string         `json:"address"`
		Mode            string         `json:"mode"`
		Type            string         `json:"type"`
		ProviderName    string         `json:"provider_name"`
		Values          map[string]any `json:"values"`
		Identity        map[string]any `json:"identity"`
		SensitiveValues map[string]any `json:"sensitive_values"`
	} `json:"resources"`
	ChildModules []showModule `json:"child_modules"`
}

// Index looks up state instances by type and attribute values.
type Index struct {
	byValue      map[string][]*Instance // type \x00 key \x00 value
	count        int
	noIdentity   int
	owned        map[string][]ownedBy // type \x00 owned-object key
	snapshots    []Snapshot
	snapshotKeys map[string]int
	typeCounts   map[TypeKey]int
	types        map[string]int  // instances per type
	root         map[string]bool // resource addresses in root modules
}

func NewIndex() *Index {
	return &Index{byValue: map[string][]*Instance{}, root: map[string]bool{}, owned: map[string][]ownedBy{}, types: map[string]int{}}
}

// RootAddresses returns the <type>.<name> addresses that states already use
// in root modules; new import blocks must not target them.
func (ix *Index) RootAddresses() map[string]bool { return ix.root }

// Values returns the distinct values of a top-level identity key or
// attribute (zone_id) of the instances of a type ("" for all), sorted.
func (ix *Index) Values(typ, key string) []string {
	seen := map[string]bool{}
	for k := range ix.byValue {
		parts := strings.SplitN(k, "\x00", 3)
		if len(parts) == 3 && parts[1] == key && (typ == "" || parts[0] == typ) {
			seen[parts[2]] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// Len returns the number of indexed instances.
func (ix *Index) Len() int { return ix.count }

// NoIdentity returns the number of indexed instances without a resource
// identity, which can only be matched by attributes.
func (ix *Index) NoIdentity() int { return ix.noIdentity }

// Add indexes instances by every scalar identity and top-level attribute.
func (ix *Index) Add(instances []Instance) {
	for i := range instances {
		in := &instances[i]
		ix.count++
		ix.types[in.Type]++
		if in.Provider != "" && !excludedOwnership(*in) {
			if ix.typeCounts == nil {
				ix.typeCounts = map[TypeKey]int{}
			}
			ix.typeCounts[TypeKey{in.Type, model.ProviderFamily(in.Provider)}]++
		}
		if len(in.Identity) == 0 {
			ix.noIdentity++
		}
		for child, o := range owners {
			if o.parent != in.Type {
				continue
			}
			for _, ref := range o.refs(in.Attributes) {
				ix.owned[child+"\x00"+ref] = append(ix.owned[child+"\x00"+ref], ownedBy{in, o.what})
			}
		}
		if !strings.HasPrefix(in.Address, "module.") {
			addr, _, _ := strings.Cut(in.Address, "[")
			ix.root[addr] = true
		}
		seen := map[string]bool{}
		for i, m := range []map[string]any{in.Identity, in.Attributes} {
			for k, v := range m {
				// Identities can hold lists; short list or object
				// attributes are indexed too, large ones would only cost
				// memory. Scalars (long Azure IDs included) always are.
				s, ok := model.Scalar(v)
				if !ok {
					if s, ok = model.Canonical(v); !ok || (i == 1 && len(s) > 256) {
						continue
					}
				}
				if s == "" {
					continue
				}
				key := in.Type + "\x00" + k + "\x00" + s
				if !seen[key] {
					seen[key] = true
					ix.byValue[key] = append(ix.byValue[key], in)
				}
			}
		}
	}
}

// Match is the result of looking a live object up in state.
type Match struct {
	Address string
	Managed bool
	// Verified is false when some identity key of the live object (an
	// account, subscription or region) is missing on the state instance,
	// from its identity or, for state without one, from its attributes: the
	// object may belong to another account with an equal ID.
	Verified bool
	// ByIdentity is set when the state instance has a resource identity;
	// otherwise the match compared attributes.
	ByIdentity bool
	// The rest is for -explain. Via is "identity", "attributes" or "owner"
	// (a resource that creates the object, Address names it); Type the
	// state type when it is an alternative name; Source where the state
	// instance was read.
	Via    string
	Type   string
	Source string
	// Unconfirmed are the live object's identity keys that state does not
	// hold; ScopeUnknown says that state cannot tell the object's cluster
	// or account (Proxmox, plugins marked unverified).
	Unconfirmed  []string
	ScopeUnknown bool
	// Unknown are the live object's keys that discovery left empty
	// (project ""), so they cannot confirm anything either.
	Unknown []string
}

// Rejection is a state instance that shares a key with a live object but
// does not manage it, and why.
type Rejection struct {
	Address, Source, Why string
}

// Explain is Match plus the instances that came close (at most limit) and
// the number of state instances of the type.
func (ix *Index) Explain(r model.Resource, limit int) (Match, []Rejection, int) {
	var rejected []Rejection
	m := ix.match(r, func(in *Instance, why string) {
		if len(rejected) < limit {
			rejected = append(rejected, Rejection{in.Address, in.Source, why})
		}
	})
	// No candidate at all (they are looked up by the rarest key): show
	// instances that share any one key value, and why they do not match,
	// with the checks of match.
	if !m.Managed && len(rejected) == 0 {
		rejected = ix.nearby(r, limit)
	}
	n := ix.types[r.Type]
	for _, t := range r.AltTypes {
		n += ix.types[t]
	}
	return m, rejected, n
}

// Managed returns the address of the state instance that manages r.
func (ix *Index) Managed(r model.Resource) (string, bool) {
	m := ix.Match(r)
	return m.Address, m.Managed
}

// nearby returns up to limit instances of r's type or its alternative
// names that share one identity value with r, and why they do not manage it.
func (ix *Index) nearby(r model.Resource, limit int) []Rejection {
	var out []Rejection
	seen := map[*Instance]bool{}
	for _, typ := range append([]string{r.Type}, r.AltTypes...) {
		as := r
		if id, ok := r.AltIdentity[typ]; ok {
			as.Identity, as.RequiredKeys, as.AttributeKeys = id, nil, nil
		}
		for _, k := range model.SortedKeys(as.Identity) {
			v, ok := model.Scalar(as.Identity[k])
			if !ok {
				v, ok = model.Canonical(as.Identity[k])
			}
			if !ok || v == "" {
				continue
			}
			for _, in := range ix.byValue[typ+"\x00"+k+"\x00"+v] {
				if len(out) == limit {
					return out
				}
				if seen[in] {
					continue
				}
				seen[in] = true
				why := ""
				ok, _, w, _ := compare(as, in)
				switch {
				case typ == r.Type && !model.SameProvider(r.ProviderSource, in.Provider):
					why = fmt.Sprintf("provider %s, not %s", orUnknown(in.Provider), orUnknown(r.ProviderSource))
				case !ok:
					why = w
				default:
					why = "in another place (node or cluster) than the scanned one"
				}
				if typ != r.Type {
					why = "as " + typ + ": " + why
				}
				out = append(out, Rejection{in.Address, in.Source, why})
			}
		}
	}
	return out
}

// Match looks r up, preferring a verified match over an unverified one.
//
// An instance matches when it has the same type and every identity attribute
// of r that is also set on the instance has the same value; required keys
// must be present. The instance's identity is used when state has one
// (Terraform 1.12+ with an identity-aware provider), otherwise its
// attributes, which covers older state and plugin results.
func (ix *Index) Match(r model.Resource) Match { return ix.match(r, nil) }

// match is Match; rejected, when set, hears of each candidate that does
// not manage r.
func (ix *Index) match(r model.Resource, rejected func(in *Instance, why string)) Match {
	reject := func(typ string, in *Instance, why string) {
		if rejected != nil {
			if typ != r.Type {
				why = "as " + typ + ": " + why
			}
			rejected(in, why)
		}
	}
	tried := map[*Instance]bool{}
	var best Match
	for _, typ := range append([]string{r.Type}, r.AltTypes...) {
		as := r
		if id, ok := r.AltIdentity[typ]; ok {
			as.Identity, as.RequiredKeys, as.AttributeKeys = id, nil, nil
		}
		for _, in := range ix.candidates(typ, as) {
			if tried[in] {
				continue
			}
			tried[in] = true
			// AltTypes are declared equivalents, possibly of another
			// provider (telmate's proxmox_vm_qemu); the type itself must
			// come from the same provider.
			if typ == r.Type && !model.SameProvider(r.ProviderSource, in.Provider) {
				reject(typ, in, fmt.Sprintf("provider %s, not %s", orUnknown(in.Provider), orUnknown(r.ProviderSource)))
				continue
			}
			ok, verified, why, missing := compare(as, in)
			if !ok {
				reject(typ, in, why)
				continue
			}
			scopeUnknown := false
			if r.Scope != nil {
				inScope, known := scopeCheck(r.Scope, typ, in)
				if !inScope {
					reject(typ, in, "in another place (node or cluster) than the scanned one")
					continue
				}
				verified, scopeUnknown = verified && known, !known
			}
			m := Match{Address: in.Address, Managed: true, Verified: verified, ByIdentity: len(in.Identity) > 0,
				Via: "attributes", Source: in.Source, Unconfirmed: missing, ScopeUnknown: scopeUnknown}
			if !verified {
				m.Unknown = unknownKeys(as)
			}
			if m.ByIdentity {
				m.Via = "identity"
			}
			if typ != r.Type {
				m.Type = typ
			}
			switch {
			case verified:
				return m
			case !best.Managed:
				best = m
			}
		}
	}
	// An unverified match does not stop a verified one through an owner.
	if !best.Verified {
		if o, ok := owners[r.Type]; ok {
			full, short := o.key(r.Identity)
			for _, k := range []string{full, short} {
				if k == "" {
					continue
				}
				for _, by := range ix.owned[r.Type+"\x00"+k] {
					// The parent must pass the checks a state instance of
					// the object would: its provider and the object's scope.
					if !model.SameProvider(r.ProviderSource, by.parent.Provider) {
						continue
					}
					verified, scopeUnknown := k == full, false
					if r.Scope != nil {
						inScope, known := scopeCheck(r.Scope, by.parent.Type, by.parent)
						if !inScope {
							continue
						}
						verified, scopeUnknown = verified && known, !known
					}
					m := Match{Address: by.parent.Address + "." + by.what, Managed: true, Verified: verified, Via: "owner", Source: by.parent.Source, ScopeUnknown: scopeUnknown}
					if verified {
						return m
					}
					m.Unknown = unknownKeys(r)
					if !best.Managed {
						best = m
					}
				}
			}
		}
	}
	return best
}

// ownedBy is the managed instance that owns an object (see owners).
type ownedBy struct {
	parent *Instance
	what   string
}

// owner describes objects that a managed resource of another type creates
// and owns, which therefore have no state of their own: a boot disk that a
// google_compute_instance creates from initialize_params. A boot disk given
// as source, and disks attached with attached_disk, are not owned: they are
// made, and may be managed or not, elsewhere.
type owner struct {
	parent string
	what   string // the parent's attribute, for the reported address
	// refs returns the owned objects' keys from the parent's attributes,
	// each in a full and a short form.
	refs func(attrs map[string]any) []string
	// key returns the full key of a live object, or the short one when its
	// project is not known; the short one cannot tell projects apart, so
	// a match by it is unverified.
	key func(identity map[string]any) (full, short string)
}

var owners = map[string]owner{
	"google_compute_disk": {
		parent: "google_compute_instance",
		what:   "boot_disk",
		refs: func(attrs map[string]any) []string {
			var out []string
			list, _ := attrs["boot_disk"].([]any)
			for _, b := range list {
				m, _ := b.(map[string]any)
				// Only a disk the instance created from initialize_params is
				// owned by it; one given as source was made elsewhere.
				if params, _ := m["initialize_params"].([]any); len(params) == 0 {
					continue
				}
				src, _ := m["source"].(string)
				// A self link: .../projects/P/zones/Z/disks/N.
				if i := strings.Index(src, "projects/"); i >= 0 {
					full := src[i:]
					out = append(out, full)
					if _, short, ok := strings.Cut(full, "/zones/"); ok {
						out = append(out, "zones/"+short)
					}
				}
			}
			return out
		},
		key: func(id map[string]any) (string, string) {
			name, _ := model.Scalar(id["name"])
			zone, _ := model.Scalar(id["zone"])
			project, _ := model.Scalar(id["project"])
			if name == "" || zone == "" {
				return "", ""
			}
			short := "zones/" + zone + "/disks/" + name
			if project == "" {
				return "", short
			}
			// A known project must match: the short key is for an
			// unknown one only.
			return "projects/" + project + "/" + short, ""
		},
	},
}

// emptyIsUnknown are identity keys of an object's location, whose empty
// value means unknown rather than a value to compare.
var emptyIsUnknown = map[string]bool{"project": true, "account_id": true, "subscription_id": true, "region": true, "zone": true, "location": true}

// UnknownLocation reports whether an identity has an empty location key.
func UnknownLocation(identity map[string]any) bool {
	for k := range emptyIsUnknown {
		if v, ok := identity[k].(string); ok && v == "" {
			return true
		}
	}
	return false
}

// compare tells whether the instance manages r and whether that is
// verified; otherwise why not. missing are the keys of r the instance does
// not hold (it then matches unverified at best).
func compare(r model.Resource, in *Instance) (ok, verified bool, why string, missing []string) {
	// Attribute names only loosely follow identity names (AWS identities
	// carry account_id, which is not an attribute), so with the attribute
	// fallback a missing secondary key is tolerated; a conflicting value
	// never is, and every primary key (the object's own ID, not its zone or
	// account) has to be present and equal. When unsure this reports an
	// object as unmanaged, never the other way round.
	src, strict := in.Identity, true
	if len(src) == 0 {
		src, strict = in.Attributes, false
	}
	required, primary := keySets(r)
	isRequired, isPrimary := map[string]bool{}, map[string]bool{}
	for _, k := range required {
		isRequired[k] = true
	}
	for _, k := range primary {
		isPrimary[k] = true
	}
	equal, primaryEqual, primaryEmpty, absent := 0, 0, 0, 0
	for k, want := range r.Identity {
		w, ok := model.Canonical(want)
		if !ok {
			// A null the source itself requires (a plugin's "id": null
			// from jq on a missing field) leaves the object unnamed: the
			// rest, a location, alone is no match.
			if slices.Contains(r.RequiredKeys, k) {
				return false, false, k + " is null here, which nothing can confirm", nil
			}
			continue
		}
		// An empty location (google lists some objects with project "")
		// is not known, so it neither conflicts nor confirms. An empty own
		// ID (name, id) is no evidence of anything: it must not let the
		// location alone match.
		if w == "" && emptyIsUnknown[k] {
			absent++
			if isPrimary[k] {
				primaryEmpty++
			}
			continue
		}
		// A key the match must rest on (required with an identity in
		// state, primary with attributes) that the object leaves empty
		// cannot be confirmed: the rest, a location, alone is no match.
		// The index finds no candidate then either; this keeps the rule
		// independent of it.
		if w == "" && (strict && isRequired[k] || !strict && isPrimary[k]) {
			return false, false, k + " is empty here, which nothing can confirm", nil
		}
		got, ok := model.Canonical(src[k])
		if !ok {
			if strict && isRequired[k] {
				return false, false, "its identity has no " + k, nil
			}
			absent++
			missing = append(missing, k)
			continue
		}
		if got != w {
			// Identities are never sensitive; attributes may be, and only
			// a sensitive id is known here, so their values stay unsaid.
			if !strict {
				return false, false, fmt.Sprintf("its attribute %s differs from %s here", k, short(w)), nil
			}
			return false, false, fmt.Sprintf("%s is %s there, %s here", k, short(got), short(w)), nil
		}
		equal++
		if isPrimary[k] {
			primaryEqual++
		}
	}
	if strict {
		// An identity in state that lacks a key of the live object (an
		// account, a region) cannot confirm where the object lives.
		if equal == 0 {
			return false, false, "no identity key in common", nil
		}
		slices.Sort(missing)
		return true, absent == 0, "", missing
	}
	ok = primaryEqual > 0 && primaryEqual+primaryEmpty == len(isPrimary)
	if !ok {
		keys := slices.Sorted(maps.Keys(isPrimary))
		return false, false, "no identity, and its attributes do not hold all of " + strings.Join(keys, ", "), nil
	}
	slices.Sort(missing)
	return true, absent == 0, "", missing
}

// unknownKeys are the keys of r whose value discovery left empty where
// empty means unknown (see emptyIsUnknown).
func unknownKeys(r model.Resource) []string {
	var out []string
	for k, v := range r.Identity {
		if s, ok := model.Canonical(v); ok && s == "" && emptyIsUnknown[k] {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// short bounds a value quoted in an explanation.
func short(s string) string {
	if len(s) > 80 {
		s = strings.ToValidUTF8(s[:80], "") + "..."
	}
	return s
}

func orUnknown(provider string) string {
	if provider == "" {
		return "unknown"
	}
	return provider
}

// candidates returns the instances that can match r. A match has to equal
// r on every required key (identity in state) or every primary key
// (attributes), so the shortest list of one required key plus the shortest
// of one primary key holds every possible match. Looking candidates up by
// the first key instead (account_id in AWS identities) would compare each
// object with every other object of its account.
func (ix *Index) candidates(typ string, r model.Resource) []*Instance {
	required, primary := keySets(r)
	shortest := func(keys []string) []*Instance {
		var best []*Instance
		found := false
		for _, k := range keys {
			v, ok := model.Canonical(r.Identity[k])
			if !ok || v == "" && emptyIsUnknown[k] {
				continue
			}
			list := ix.byValue[typ+"\x00"+k+"\x00"+v]
			if !found || len(list) < len(best) {
				best, found = list, true
			}
		}
		return best
	}
	a, b := shortest(required), shortest(primary)
	if len(b) == 0 {
		return a
	}
	if len(a) == 0 {
		return b
	}
	out := slices.Clone(a)
	seen := map[*Instance]bool{}
	for _, in := range a {
		seen[in] = true
	}
	for _, in := range b {
		if !seen[in] {
			out = append(out, in)
		}
	}
	return out
}

// keySets returns the keys a match must equal: required ones when state
// has an identity, primary ones when it is matched by attributes.
func keySets(r model.Resource) (required, primary []string) {
	required = r.RequiredKeys
	if len(required) == 0 {
		required = model.SortedKeys(r.Identity)
	}
	primary = r.RequiredKeys
	if r.AttributeKeys != nil {
		primary = r.AttributeKeys
	} else if len(primary) == 0 {
		if _, ok := r.Identity["id"]; ok {
			primary = []string{"id"}
		} else {
			primary = required
		}
	}
	return required, primary
}

// scopeCheck applies a model.Scope to a state instance: whether the instance
// can belong to the scanned place at all, and whether that is known. A
// location the place does not have rules the instance out; a location it
// has proves nothing (node names repeat across clusters), so only the
// user's statement (Trusted) makes the match verified.
func scopeCheck(sc *model.Scope, typ string, in *Instance) (inScope, known bool) {
	known = true
	for ; sc != nil; sc = sc.Also {
		if !sc.Unverifiable {
			if attr, ok := sc.Attrs[typ]; ok {
				if v, ok := model.Scalar(in.Attributes[attr]); ok && !sc.Values[v] {
					return false, false
				}
			}
		}
		known = known && sc.Trusted
	}
	return true, known
}

// TypeKey is a resource type with the provider family that manages it.
type TypeKey struct {
	Type, Provider string
}

// TypeCounts counts the managed instances per type and provider family.
// Instances without a provider address and those of providers that create
// no remote object (random, null, local, ...) are left out.
func (ix *Index) TypeCounts() map[TypeKey]int { return ix.typeCounts }
