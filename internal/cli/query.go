package cli

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/report"
	"github.com/5xxeu/tforphans/internal/state"
	"github.com/5xxeu/tforphans/internal/tf"
)

// scanRoot adds the root's state to idx and returns the live resources its
// providers list.
func scanRoot(ctx context.Context, o *options, root string, idx *state.Index, sum *report.Summary, stdout io.Writer) ([]model.Resource, error) {
	// Leftovers of a killed run must go before the configuration is read:
	// its provider override would count as a declaration.
	if err := tf.RemoveStale(root); err != nil {
		return nil, err
	}
	r := tf.Runner{Bin: o.src.terraformBin, Dir: root, AnyVersion: o.src.anyVersion, MaxQuerySize: o.src.maxQuerySize, MaxStateSize: o.src.maxStateSize}
	initialized := r.Initialized()
	if !initialized && o.src.init {
		if err := r.Init(ctx); err != nil {
			return nil, err
		}
	}
	// A root that is not initialized and fails for it: as for
	// -state-roots, the other roots still run, and the gap shows in
	// coverage and for -strict. (Without .terraform a root may still work:
	// no providers, or providers from dev_overrides.)
	uninitialized := func(err error) bool {
		// A timeout or cancel is no sign of a missing init.
		if initialized || o.src.init || ctx.Err() != nil {
			return false
		}
		u := report.NewUnreadState(root, err)
		u.Error = "not initialized, so not queried either (run terraform init there, or pass -init): " + u.Error
		sum.UnreadStates = append(sum.UnreadStates, u)
		if !o.src.noQuery {
			sum.UnqueriedRoots = append(sum.UnqueriedRoots, root)
		}
		return true
	}
	if o.src.noQuery {
		// Only the state is needed: no provider schemas or query files.
		snapshots, n, err := pullRoot(ctx, o, root)
		// The workspaces that were read still count.
		idx.AddSnapshots(snapshots)
		sum.States += n
		if err != nil && !uninitialized(err) {
			if ctx.Err() != nil {
				return nil, err
			}
			sum.UnreadStates = append(sum.UnreadStates, report.NewUnreadState(root, err))
		}
		return nil, nil
	}
	schemas, err := r.ProvidersSchema(ctx)
	if err != nil {
		if uninitialized(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w\n(is the module initialized? run terraform init)", err)
	}
	sum.MergeProviders(schemas, nil)
	for addr, ps := range schemas.ProviderSchemas {
		sum.AddListable(addr, slices.Collect(maps.Keys(ps.ListResourceSchemas))...)
	}
	declared, err := tf.LocalNames(root)
	if err != nil {
		return nil, err
	}
	providerConfigs, err := tf.ProviderSourceConfigs(root)
	if err != nil {
		return nil, err
	}
	configs, err := tf.ProviderConfigs(root)
	if err != nil {
		return nil, err
	}
	plan := tf.BuildPlan(schemas, declared, tf.PlanOptions{Args: o.src.args, Include: o.src.include, Exclude: o.src.exclude, Configs: providerConfigs, NoAliases: o.src.noAliases,
		IncludeResource: func(t string) bool { return len(o.wantedAttrs(t)) > 0 || o.adopt.tagNames() && hasTags(schemas, t) }, ProviderConfigs: configs})
	for _, sk := range plan.Skipped {
		sk.Root = root
		sum.Skipped = append(sum.Skipped, sk)
	}
	for _, n := range plan.Notes {
		sum.Notes = append(sum.Notes, root+": "+n)
	}
	sum.NoList = append(sum.NoList, plan.NoList...)
	if o.src.printQuery {
		return nil, printQueryFiles(stdout, root, plan, o.src.limit)
	}

	snapshots, n, err := pullRoot(ctx, o, root)
	// The workspaces that were read still count; an unread one is a gap,
	// as with -state-roots, and the root is still queried.
	idx.AddSnapshots(snapshots)
	sum.States += n
	if err != nil {
		if uninitialized(err) {
			return nil, nil
		}
		if ctx.Err() != nil {
			return nil, err
		}
		sum.UnreadStates = append(sum.UnreadStates, report.NewUnreadState(root, err))
	}

	// The user's own list blocks run too, even when nothing is generated.
	userLists, err := tf.UserLists(root)
	if err != nil {
		return nil, err
	}
	if o.src.noQuery || len(plan.Queries) == 0 && len(userLists) == 0 {
		return nil, nil
	}
	// Generated blocks are the plan's own addresses: a block of the user's
	// whose label happens to start with tfo_ is still the user's.
	byAddress := map[string]tf.Query{}
	for _, q := range plan.Queries {
		byAddress[q.Address()] = q
	}
	// Generated blocks use -limit; the user's blocks their own limit.
	limitFor := func(block string) int {
		if _, ok := byAddress[block]; block == "" || ok {
			return o.src.limit
		}
		return userLists[block].Limit
	}
	// Identity keys come from the schema of the provider that listed the
	// object (google and google-beta share types).
	idKeys := schemas.IdentityKeys()
	keysFor := func(block, typ string) ([]string, []string) {
		if q, ok := byAddress[block]; ok {
			return idKeys.For(q.Source, typ)
		}
		return idKeys.For(userListProvider(userLists[block].Provider, typ, declared, schemas), typ)
	}
	// Whole objects are large; keep the attributes asked for, as each
	// object is read. Generated blocks ask for them; the user's own blocks
	// do only with their own include_resource = true.
	noAttrs := map[string]bool{}
	var noAttrsOrder []string
	trim := func(f *model.Resource) {
		if f.Attributes == nil {
			block, _, _ := strings.Cut(f.ListAddress, "[")
			tags := o.adopt.tagNames() && hasTags(schemas, f.Type)
			if _, generated := byAddress[block]; !generated && (len(o.wantedAttrs(f.Type)) > 0 || tags) && !noAttrs[block] {
				noAttrs[block] = true
				noAttrsOrder = append(noAttrsOrder, block)
			}
			return
		}
		if o.adopt.tagNames() {
			f.TagName = nameTag(f.Attributes)
		}
		f.Attributes = keepAttrs(f.Attributes, o.wantedAttrs(f.Type))
	}
	res, err := r.Query(ctx, plan, limitFor, keysFor, trim)
	if err != nil {
		return nil, queryError{err}
	}
	for _, block := range noAttrsOrder {
		sum.Notes = append(sum.Notes, fmt.Sprintf("%s: %s has no include_resource = true, so its results carry no attributes for -include-attrs, attr. ignore rules or -names-from-tags", root, block))
	}
	sum.Queried += len(plan.Queries) + len(userLists)
	// providerOf is the provider configuration a list block instance
	// used (aws, aws.prod): coverage counts each configuration apart.
	providerOf := func(address, typ string) string {
		block, _, _ := strings.Cut(address, "[")
		if q, ok := byAddress[block]; ok {
			return q.Ref()
		}
		return userLists[block].ProviderRef(typ)
	}
	// localOf is its local name (the provider, whatever the alias).
	localOf := func(address, typ string) string {
		local, _, _ := strings.Cut(providerOf(address, typ), ".")
		return local
	}
	providerSource := func(address, typ string) string {
		block, _, _ := strings.Cut(address, "[")
		source := byAddress[block].Source
		if source == "" {
			source = userListProvider(userLists[block].Provider, typ, declared, schemas)
			if source == "" {
				addr := "registry.terraform.io/hashicorp/" + localOf(address, typ)
				if _, ok := schemas.ProviderSchemas[addr]; ok {
					source = addr
				}
			}
		}
		return source
	}
	var identityConfigs []tf.IdentityConfig
	for _, q := range res.Queried {
		identityConfigs = append(identityConfigs, tf.IdentityConfig{Ref: providerOf(q.Address, q.Type), Source: providerSource(q.Address, q.Type)})
	}
	scopes, failed, err := r.Identities(ctx, plan, identityConfigs)
	if err != nil {
		return nil, queryError{err}
	}
	refs := make([]string, 0, len(failed))
	for ref := range failed {
		refs = append(refs, ref)
	}
	slices.Sort(refs)
	for _, ref := range refs {
		sum.Notes = append(sum.Notes, fmt.Sprintf("%s: could not read where provider configuration %s looks (%s); missing findings of its listings stay unverified with -baseline", root, ref, tf.OneLine(failed[ref].Error(), 300)))
	}
	workspace := tf.SelectedWorkspace(root)

	// Only failures of this instance or its block count against it.
	finished := map[string]bool{}
	for _, d := range res.Done {
		finished[d.Address] = true
	}
	queried := map[string][]string{}
	for _, q := range res.Queried {
		source := providerSource(q.Address, q.Type)
		queried[source] = append(queried[source], q.Type)
		block, key, _ := strings.Cut(q.Address, "[")
		key = strings.TrimSuffix(key, "]")
		if value, err := strconv.Unquote(key); err == nil {
			key = value
		}
		config, provider := userLists[block].Configuration, userLists[block].Provider
		if provider == "" {
			provider = providerOf(q.Address, q.Type)
		}
		if generated, ok := byAddress[block]; ok {
			config = string(generated.HCL(o.src.limit))
			provider = generated.Ref()
		}
		complete := finished[q.Address]
		for _, f := range res.Failed {
			if o.listingFailure(f) && (f.Address == q.Address || f.Address == block) {
				complete = false
			}
		}
		scope := scopes[providerOf(q.Address, q.Type)]
		if user, ok := userLists[block]; ok && !user.Static {
			scope = nil
		}
		if _, optIn := tf.OptIn[q.Type]; optIn {
			// These listings follow credential visibility, which provider
			// identity data sources do not describe.
			scope = nil
		}
		sum.AddListing(report.Listing{Source: root, Address: q.Address, Provider: source, Type: q.Type,
			Complete: complete, Scope: scope, Fingerprint: report.ListingFingerprint(config, key, provider, "", workspace)})
	}
	sum.MergeProviders(schemas, queried)
	// What finished counts, not what was planned: a block with
	// for_each = {} checks nothing. The objects found give their own rows
	// (project, account); a root that listed a type and found none of it
	// gets a row of its own when there are several roots. A failure keeps
	// no scope and so concerns every row of its type: the objects of a
	// listing cut short may sit in any of them.
	found := map[string]bool{}
	for _, f := range res.Found {
		found[f.Type] = true
	}
	for _, d := range res.Done {
		rootScope := ""
		if len(o.src.roots) > 1 && !found[d.Type] {
			rootScope = "root " + root
		}
		sum.Checks = append(sum.Checks, tf.Check{Type: d.Type, Scope: rootScope, Root: root, Provider: providerOf(d.Address, d.Type)})
	}
	sum.Ran += res.Started
	for i := range res.Failed {
		res.Failed[i].Root = root
		res.Failed[i].Provider = providerOf(res.Failed[i].Address, res.Failed[i].Type)
	}
	sum.Failed = append(sum.Failed, res.Failed...)
	// Import blocks have to use the provider configuration the object was
	// listed with: ours for generated blocks, the block's own (possibly an
	// alias such as aws.other_account) for the user's list blocks.
	for i := range res.Found {
		f := &res.Found[i]
		block, _, _ := strings.Cut(f.ListAddress, "[")
		q, generated := byAddress[block]
		f.ProviderSource = providerSource(f.ListAddress, f.Type)
		if !generated {
			// A block without a provider argument uses the type's default
			// provider, and so does an import block without one.
			f.Provider = userLists[block].Provider
			continue
		}
		switch {
		case q.Temporary:
			f.UndeclaredProvider = q.Source
		case q.Alias != "":
			f.Provider = q.Ref()
		case !strings.HasPrefix(q.Type, q.Provider+"_") && q.Type != q.Provider:
			f.Provider = q.Provider
		}
	}
	return res.Found, nil
}

// wantedAttrs returns the attributes to keep for a type: -include-attrs and
// those the ignore rules look at.
func (o *options) wantedAttrs(typ string) []string {
	out := slices.Clone(o.src.includeAttrs)
	if o.run.ignore != nil {
		for _, a := range o.run.ignore.Attributes(typ) {
			if !slices.Contains(out, a) {
				out = append(out, a)
			}
		}
	}
	return out
}

// trimAttrs keeps the attributes asked for with -include-attrs.
// keepAttrs keeps of a listed object's attributes those wanted (for
// -include-attrs and attr. ignore rules) and its kind, the type attribute,
// which names it in ignore hints; trimAttrs drops that from the output
// unless asked for.
func keepAttrs(attrs map[string]any, wanted []string) map[string]any {
	keep := map[string]any{}
	for _, a := range wanted {
		if v, ok := attrs[a]; ok {
			keep[a] = v
		}
	}
	if v, ok := attrs["type"]; ok {
		if _, scalar := model.Scalar(v); scalar {
			keep["type"] = v
		}
	}
	if len(keep) == 0 {
		return nil
	}
	return keep
}

func (o *options) trimAttrs(r *model.Resource) {
	if r.Attributes == nil {
		return
	}
	keep := map[string]any{}
	for _, a := range o.src.includeAttrs {
		if v, ok := r.Attributes[a]; ok {
			keep[a] = v
		}
	}
	r.Attributes = nil
	if len(keep) > 0 {
		r.Attributes = keep
	}
}

// userListProvider resolves the provider address of a user's list block
// from its provider argument ("aws", "aws.other"; empty means the type's
// prefix, as terraform infers it): the root's required_providers, else the
// implicit hashicorp provider. "" when it cannot be told.
func userListProvider(arg, typ string, declared map[string]string, schemas *tf.Schemas) string {
	local, _, _ := strings.Cut(arg, ".")
	if local == "" {
		local, _, _ = strings.Cut(typ, "_")
	}
	for addr, name := range declared {
		if name == local {
			return addr
		}
	}
	if addr := "registry.terraform.io/hashicorp/" + local; schemas.ProviderSchemas[addr].ResourceIdentitySchemas != nil {
		return addr
	}
	return ""
}

// listingFailure follows coverage's API-disabled exception (-strict-api=false). -strict-ignore
// affects exit status, but cannot make a failed listing resolution evidence.
func (o *options) listingFailure(f tf.Failure) bool {
	f.Classify()
	return f.Category != tf.FailAPIOff || o.audit.strictAPI
}

// printQueryFiles prints what a run writes into the root, with argument
// credentials redacted, one temporary file after the other:
// terraform reads .tf and .tfquery.hcl differently,
// so they must not run together into one document.
func printQueryFiles(w io.Writer, root string, plan tf.Plan, limit int) error {
	plan.Queries = slices.Clone(plan.Queries)
	for i := range plan.Queries {
		q := &plan.Queries[i]
		q.Args = slices.Clone(q.Args)
		for j := range q.Args {
			a := &q.Args[j]
			a.Values = slices.Clone(a.Values)
			for k, value := range a.Values {
				_, a.Values[k], _ = strings.Cut(redactArg(a.Name+"="+value), "=")
			}
		}
	}
	// A failed write must fail the run: the files are meant to be saved.
	if _, err := fmt.Fprintf(w, "# root %s\n", report.Clean(root)); err != nil {
		return err
	}
	for _, f := range []struct{ name, text string }{
		{"zz_tforphans_<pid>_override.tf", string(plan.ProvidersHCL())},
		{"zz_tforphans_<pid>_auto.tf", string(plan.AutoHCL())},
		{"zz_tforphans_<pid>.tfquery.hcl", string(plan.HCL(limit))},
	} {
		if strings.TrimSpace(f.text) != "" {
			if _, err := fmt.Fprintf(w, "\n# file %s\n%s", f.name, f.text); err != nil {
				return err
			}
		}
	}
	return nil
}

// hasTags reports whether some provider's schema of the type has a tags
// attribute (AWS, Azure): only those types are fetched whole for
// -names-from-tags.
func hasTags(s *tf.Schemas, typ string) bool {
	for _, ps := range s.ProviderSchemas {
		if rs, ok := ps.ResourceSchemas[typ]; ok {
			if _, ok := rs.Block.Attributes["tags"]; ok {
				return true
			}
		}
	}
	return false
}

// nameTag is the Name tag of a listed object, "" when it has none.
func nameTag(attrs map[string]any) string {
	tags, _ := attrs["tags"].(map[string]any)
	name, _ := tags["Name"].(string)
	return strings.TrimSpace(name)
}

// queryError is a failure of terraform query itself, after the root's
// state was read: the root is not covered, but its objects are known.
type queryError struct{ err error }

func (e queryError) Error() string { return e.err.Error() }
func (e queryError) Unwrap() error { return e.err }
