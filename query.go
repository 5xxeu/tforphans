package main

import (
	"context"
	"fmt"
	"io"
	"slices"
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
	if !r.Initialized() && o.init {
		if err := r.Init(ctx); err != nil {
			return nil, err
		}
	}
	if o.noQuery {
		// Only the state is needed: no provider schemas or query files.
		insts, n, err := pullRoot(ctx, o, root)
		if err != nil {
			return nil, err
		}
		idx.Add(insts)
		sum.States += n
		return nil, nil
	}
	schemas, err := r.ProvidersSchema(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w\n(is the module initialized? run terraform init)", err)
	}
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
		fmt.Fprintf(stdout, "# %s\n%s%s", root, plan.ProvidersHCL(), plan.HCL(o.limit))
		return nil, nil
	}

	insts, n, err := pullRoot(ctx, o, root)
	if err != nil {
		return nil, err
	}
	idx.Add(insts)
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
		keep := map[string]any{}
		for _, a := range o.wantedAttrs(f.Type) {
			if v, ok := f.Attributes[a]; ok {
				keep[a] = v
			}
		}
		f.Attributes = nil
		if len(keep) > 0 {
			f.Attributes = keep
		}
	}
	sum.Queried += len(plan.Queries) + len(userLists)
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
		block, _, _ := strings.Cut(d.Address, "[")
		local := ""
		if q, ok := byAddress[block]; ok {
			local = q.Provider
		} else if local, _, _ = strings.Cut(userLists[block].Provider, "."); local == "" {
			local, _, _ = strings.Cut(d.Type, "_")
		}
		sum.Checks = append(sum.Checks, tf.Check{Type: d.Type, Scope: rootScope, Root: root, Provider: local})
	}
	sum.Ran += res.Started
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
