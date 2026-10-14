package main

import (
	"context"
	"fmt"
	"io"
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
	r := tf.Runner{Bin: o.terraformBin, Dir: root, AnyVersion: o.anyVersion}
	initialized := r.Initialized()
	if !initialized && o.init {
		if err := r.Init(ctx); err != nil {
			return nil, err
		}
	}
	// A root that is not initialized and fails for it: as for
	// -state-roots, the other roots still run, and the gap shows in
	// coverage and for -strict. (Without .terraform a root may still work:
	// no providers, or providers from dev_overrides.)
	uninitialized := func(err error) bool {
		if initialized || o.init {
			return false
		}
		u := report.NewUnreadState(root, err)
		u.Error = "not initialized, so not queried either (run terraform init there, or pass -init): " + u.Error
		sum.UnreadStates = append(sum.UnreadStates, u)
		if !o.noQuery {
			sum.UnqueriedRoots = append(sum.UnqueriedRoots, root)
		}
		return true
	}
	if o.noQuery {
		// Only the state is needed: no provider schemas or query files.
		snapshots, n, err := pullRoot(ctx, o, root)
		if err != nil {
			if uninitialized(err) {
				return nil, nil
			}
			return nil, err
		}
		idx.AddSnapshots(snapshots)
		sum.States += n
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
	declared, err := tf.LocalNames(root)
	if err != nil {
		return nil, err
	}
	aliasOnly, err := tf.AliasOnly(root)
	if err != nil {
		return nil, err
	}
	configs, err := tf.ProviderConfigs(root)
	if err != nil {
		return nil, err
	}
	plan := tf.BuildPlan(schemas, declared, tf.PlanOptions{Args: o.args, Include: o.include, Exclude: o.exclude, AliasOnly: aliasOnly,
		IncludeResource: func(t string) bool { return len(o.wantedAttrs(t)) > 0 }, ProviderConfigs: configs})
	for _, sk := range plan.Skipped {
		sk.Root = root
		sum.Skipped = append(sum.Skipped, sk)
	}
	for _, n := range plan.Notes {
		sum.Notes = append(sum.Notes, root+": "+n)
	}
	sum.NoList = append(sum.NoList, plan.NoList...)
	if o.printQuery {
		fmt.Fprintf(stdout, "# %s\n%s%s%s", root, plan.ProvidersHCL(), plan.AutoHCL(), plan.HCL(o.limit))
		return nil, nil
	}

	snapshots, n, err := pullRoot(ctx, o, root)
	if err != nil {
		if uninitialized(err) {
			return nil, nil
		}
		return nil, err
	}
	idx.AddSnapshots(snapshots)
	sum.States += n

	// The user's own list blocks run too, even when nothing is generated.
	userLists, err := tf.UserLists(root)
	if err != nil {
		return nil, err
	}
	if o.noQuery || len(plan.Queries) == 0 && len(userLists) == 0 {
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
			return o.limit
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
	res, err := r.Query(ctx, plan, limitFor, keysFor)
	if err != nil {
		return nil, err
	}
	// Whole objects are large; keep the attributes asked for. Generated
	// blocks ask for them; the user's own blocks do only with their own
	// include_resource = true.
	noAttrs := map[string]bool{}
	for i := range res.Found {
		f := &res.Found[i]
		if f.Attributes == nil {
			block, _, _ := strings.Cut(f.ListAddress, "[")
			if _, generated := byAddress[block]; !generated && len(o.wantedAttrs(f.Type)) > 0 && !noAttrs[block] {
				noAttrs[block] = true
				sum.Notes = append(sum.Notes, fmt.Sprintf("%s: %s has no include_resource = true, so its results carry no attributes for -include-attrs or attr. ignore rules", root, block))
			}
			continue
		}
		f.Attributes = keepAttrs(f.Attributes, o.wantedAttrs(f.Type))
	}
	sum.Queried += len(plan.Queries) + len(userLists)
	// providerOf is the local provider name a list block instance used.
	providerOf := func(address, typ string) string {
		block, _, _ := strings.Cut(address, "[")
		if q, ok := byAddress[block]; ok {
			return q.Provider
		}
		if local, _, _ := strings.Cut(userLists[block].Provider, "."); local != "" {
			return local
		}
		local, _, _ := strings.Cut(typ, "_")
		return local
	}
	providerSource := func(address, typ string) string {
		block, _, _ := strings.Cut(address, "[")
		source := byAddress[block].Source
		if source == "" {
			source = userListProvider(userLists[block].Provider, typ, declared, schemas)
			if source == "" {
				addr := "registry.terraform.io/hashicorp/" + providerOf(address, typ)
				if _, ok := schemas.ProviderSchemas[addr]; ok {
					source = addr
				}
			}
		}
		return source
	}
	// Failures are counted per source, provider family and type. Ignoring
	// one for -strict does not prove that the listing was complete.
	incomplete := map[[2]string]bool{}
	for _, f := range res.Failed {
		if o.listingFailure(f) {
			incomplete[[2]string{listingFamily(providerSource(f.Address, f.Type)), f.Type}] = true
		}
	}
	queried := map[string][]string{}
	for _, q := range res.Queried {
		source := providerSource(q.Address, q.Type)
		queried[source] = append(queried[source], q.Type)
		sum.AddListing(root, source, q.Type, listingScope(q.Address), !incomplete[[2]string{listingFamily(source), q.Type}])
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
		if len(o.roots) > 1 && !found[d.Type] {
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
		if !generated {
			// A block without a provider argument uses the type's default
			// provider, and so does an import block without one.
			f.Provider = userLists[block].Provider
			f.ProviderSource = userListProvider(f.Provider, f.Type, declared, schemas)
			continue
		}
		f.ProviderSource = q.Source
		switch {
		case q.Temporary:
			f.UndeclaredProvider = q.Source
		case !strings.HasPrefix(q.Type, q.Provider+"_") && q.Type != q.Provider:
			f.Provider = q.Provider
		}
	}
	return res.Found, nil
}

// wantedAttrs returns the attributes to keep for a type: -include-attrs and
// those the ignore rules look at.
func (o *options) wantedAttrs(typ string) []string {
	out := slices.Clone(o.includeAttrs)
	if o.ignore != nil {
		for _, a := range o.ignore.Attributes(typ) {
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
	for _, a := range o.includeAttrs {
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

// listingFailure follows coverage's API-disabled exception. -strict-ignore
// affects exit status, but cannot make a failed listing resolution evidence.
func (o *options) listingFailure(f tf.Failure) bool {
	f.Classify()
	return f.Category != tf.FailAPIOff || o.strictAPI
}

func listingFamily(provider string) string {
	if provider == "" {
		return ""
	}
	return model.ProviderFamily(provider)
}

// listingScope reads the check scope from an actual for_each instance.
// Generated keys use argument=value pairs. Other keys are kept verbatim.
func listingScope(address string) string {
	_, key, ok := strings.Cut(address, "[")
	if !ok {
		return ""
	}
	key = strings.TrimSuffix(key, "]")
	if value, err := strconv.Unquote(key); err == nil {
		key = value
	}
	args := map[string]string{}
	for _, part := range strings.Split(key, ",") {
		name, value, ok := strings.Cut(part, "=")
		if !ok {
			return key
		}
		args[name] = value
	}
	// Keep every explicit argument, including zone or region alongside
	// project. Dropping one would turn a narrow check into broader evidence.
	var parts []string
	for _, name := range scopeKeys {
		if value, ok := args[name]; ok {
			parts = append(parts, name+" "+value)
			delete(args, name)
		}
	}
	var names []string
	for name := range args {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		parts = append(parts, name+" "+args[name])
	}
	return strings.Join(parts, ", ")
}
