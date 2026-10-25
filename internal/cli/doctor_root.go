package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/report"
	"github.com/5xxeu/tforphans/internal/state"
	"github.com/5xxeu/tforphans/internal/tf"
	"github.com/hashicorp/hcl/v2/hclparse"
)

func (d *doctorReport) version(ctx context.Context, o *options) {
	type version struct {
		Version string `json:"terraform_version"`
	}
	v, err := withSource(ctx, o, func(ctx context.Context) (version, error) {
		out, _, err := (tf.Runner{Bin: o.src.terraformBin, Dir: "."}).Exec(ctx, "version", "-json")
		var v version
		if err == nil {
			err = json.Unmarshal(out, &v)
			if err == nil && v.Version == "" {
				err = errors.New("missing terraform_version")
			}
		}
		return v, err
	})
	if err != nil {
		d.add("terraform.version", "failed", "could not read Terraform version JSON", "", "Check -terraform and retry")
		d.incomplete = true
		return
	}
	if !o.src.noQuery {
		if err := tf.CheckQueryVersion(o.src.terraformBin, v.Version); err != nil {
			d.add("terraform.version", "failed", err.Error(), "", "Install Terraform 1.14 or newer")
			return
		}
	}
	d.add("terraform.version", "ok", v.Version, "", "")
}

func (d *doctorReport) root(ctx context.Context, o *options, root string, query bool, listable map[state.TypeKey]bool) bool {
	removed, err := tf.RemoveStaleCount(root)
	if removed > 0 {
		d.add("root.stale", "warning", fmt.Sprintf("removed %d temporary files of dead tforphans processes", removed), root, "")
		d.Checks[len(d.Checks)-1].Count = &removed
	}
	if err != nil {
		d.add("root.stale", "failed", report.NewUnreadState(root, err).Error, root, "Remove zz_tforphans_* temporary files manually in "+shellQuote(root)+" after verifying no tforphans process is using them")
		return false
	}

	if o.src.terragrunt && !query {
		kind, err := tf.TerragruntKind(root)
		if err == nil && kind != tf.TerragruntNone {
			d.add("root.config", "ok", "Terragrunt unit configuration parsed", root, "")
			d.add("root.initialized", "skipped", "Terragrunt manages unit initialization", root, "")
			d.add("root.lock", "skipped", "Terragrunt selects the rendered root's lock file", root, "")
			d.add("root.query", "skipped", "used only for state", root, "")
			return true
		}
	}

	r := tf.Runner{Bin: o.src.terraformBin, Dir: root, MaxStateSize: o.src.maxStateSize}
	init := "terraform -chdir=" + shellQuote(root) + " init"
	declared, err := tf.LocalNames(root)
	// State-only roots count too: their provider may need -scan.
	d.providers = append(d.providers, slices.Collect(maps.Keys(declared))...)
	if err != nil || !tf.HasConfig(root) {
		status, message, fix := "failed", "root configuration is missing or invalid", "Check the root's Terraform configuration"
		if !query && err == nil {
			status, message, fix = "warning", "no root configuration; used only for state", ""
		}
		d.add("root.config", status, message, root, fix)
	} else {
		d.add("root.config", "ok", "root configuration parsed", root, "")
	}
	if !r.Initialized() {
		// Relative to the root, so that the same warning in many roots
		// is printed once.
		dir := tf.DataDir(root)
		if rel, err := filepath.Rel(root, dir); err == nil && filepath.IsLocal(rel) {
			dir = rel
		}
		d.add("root.initialized", "warning", "Terraform data directory is missing: "+dir, root, init)
	} else {
		d.add("root.initialized", "ok", "Terraform data directory exists (provider schemas and state access are checked separately)", root, "")
	}
	lockPath := filepath.Join(root, ".terraform.lock.hcl")
	if raw, err := os.ReadFile(lockPath); err == nil {
		_, diags := hclparse.NewParser().ParseHCL(raw, lockPath)
		if diags.HasErrors() {
			d.add("root.lock", "failed", "provider lock file is invalid", root, "terraform -chdir="+shellQuote(root)+" providers lock")
		} else {
			d.add("root.lock", "ok", "provider lock file parsed", root, "")
		}
	} else if os.IsNotExist(err) {
		d.add("root.lock", "warning", "no provider lock file", root, "terraform -chdir="+shellQuote(root)+" providers lock")
	} else {
		d.add("root.lock", "failed", "provider lock file is unreadable", root, "Check lock file permissions")
	}
	if !query {
		d.add("root.query", "skipped", "used only for state", root, "")
		return true
	}
	if o.doctor.offline && !r.Initialized() {
		d.add("providers.schema", "skipped", "offline: root is not initialized", root, init)
		d.skipPlan(root, "provider schema unavailable")
		d.add("auth.providers", "skipped", "offline: authentication disabled", root, "")
		return true
	}
	schemas, err := withSource(ctx, o, r.ProvidersSchema)
	if err != nil {
		d.add("providers.schema", "failed", "provider schema could not be read", root, init)
		if doctorResponseError(err) {
			d.incomplete = true
		}
		d.skipPlan(root, "provider schema unavailable")
		d.add("auth.providers", "skipped", "provider schema unavailable", root, "")
		return true
	}
	if schemas.ProviderSchemas == nil {
		d.add("providers.schema", "failed", "schema JSON has no provider_schemas object", root, "")
		d.incomplete = true
		d.skipPlan(root, "provider schema unavailable")
		d.add("auth.providers", "skipped", "provider schema unavailable", root, "")
		return true
	}
	if len(schemas.ProviderSchemas) == 0 {
		d.add("providers.schema", "ok", "no providers in this root", root, "")
	}
	for _, addr := range slices.Sorted(maps.Keys(schemas.ProviderSchemas)) {
		d.providers = append(d.providers, addr)
		ps := schemas.ProviderSchemas[addr]
		types := slices.Sorted(maps.Keys(ps.ListResourceSchemas))
		d.add("providers.schema", "ok", fmt.Sprintf("%s: %d list resource types", addr, len(types)), root, "")
		c := &d.Checks[len(d.Checks)-1]
		c.Provider, c.Types = addr, types
		for _, t := range types {
			listable[state.TypeKey{Type: t, Provider: model.ProviderFamily(addr)}] = true
			listable[state.TypeKey{Type: t}] = true
		}
	}
	sc, err := tf.ProviderSourceConfigs(root)
	if err != nil {
		d.add("query.plan", "failed", "provider configuration could not be parsed", root, "")
		return true
	}
	pc, err := tf.ProviderConfigs(root)
	if err != nil {
		d.add("query.plan", "failed", "provider configuration could not be parsed", root, "")
		return true
	}
	p := tf.BuildPlan(schemas, declared, tf.PlanOptions{Args: o.src.args, Include: o.src.include, Exclude: o.src.exclude, Configs: sc, NoAliases: o.src.noAliases, ProviderConfigs: pc})
	userLists, err := tf.UserLists(root)
	if err != nil {
		d.add("query.plan", "failed", "user list blocks could not be parsed", root, "Check .tfquery.hcl files")
	}
	d.queries += len(p.Queries) + len(userLists)
	planStatus := "ok"
	if len(p.Queries)+len(userLists) == 0 {
		// The audit only reads this root's state.
		planStatus = "warning"
	}
	d.add("query.plan", planStatus, fmt.Sprintf("%d generated listings and %d user list blocks selected; no listings executed", len(p.Queries), len(userLists)), root, "")
	for _, n := range p.Notes {
		d.add("query.plan", "warning", n, root, "")
	}
	for _, addr := range p.NoList {
		d.add("query.plan", "warning", addr+" has no list resources", root, "Use a supported -scan or -plugin")
	}
	for _, q := range p.Queries {
		if q.Temporary {
			d.add("query.plan", "warning", q.Source+" is declared only in child modules; listing uses an empty root configuration", root, "")
			break
		}
	}
	argsStatus, aliasStatus := "ok", "ok"
	for _, sk := range p.Skipped {
		id, status, fix := "query.plan", "warning", ""
		var argFixes []doctorArgFix
		if sk.Excluded {
			status = "skipped"
		} else if address := doctorUserListCoverage(sk, root, userLists); strings.HasPrefix(sk.Reason, tf.MissingArgs) && address != "" {
			d.planSkip("query.arguments", "skipped", "covered by user list block "+address, root, "", sk)
			continue
		} else if strings.HasPrefix(sk.Reason, tf.MissingArgs) {
			id, status, argsStatus = "query.arguments", "failed", "failed"
			missing := strings.Split(strings.TrimPrefix(sk.Reason, tf.MissingArgs), " (provider ")[0]
			for _, name := range strings.Split(missing, ", ") {
				if !strings.Contains(name, " ") && doctorStringArg(schemas, userListProvider(sk.Provider, sk.Type, declared, schemas), sk.Type, name) {
					value := "..."
					source := userListProvider(sk.Provider, sk.Type, declared, schemas)
					if source == "" {
						for addr, ps := range schemas.ProviderSchemas {
							if _, ok := ps.ListResourceSchemas[sk.Type]; !ok {
								continue
							}
							if source != "" && model.ProviderFamily(addr) != model.ProviderFamily(source) {
								source = ""
								break
							}
							source = addr
						}
					}
					if doctorAutoArg(source, name) {
						value = "auto"
					}
					argFixes = append(argFixes, doctorArgFix{typ: sk.Type, name: name, value: value})
				}
			}
			fix = "Write user list blocks supplying " + missing
			if len(argFixes) != len(strings.Split(missing, ", ")) {
				for i := range argFixes {
					argFixes[i].userList = fix
				}
			}
		} else if strings.HasPrefix(sk.Reason, "-arg ") {
			status, fix = "failed", "Supply an explicit -arg value instead of auto"
		} else if strings.HasPrefix(sk.Reason, "provider ") || strings.HasPrefix(sk.Reason, "provider configuration ") {
			id, aliasStatus = "query.aliases", "warning"
			if o.src.noAliases && strings.HasSuffix(sk.Reason, " not listed (-no-aliases)") {
				status = "warning"
			} else {
				status = "failed"
				aliasStatus = "failed"
			}
			fix = "Review provider configurations and -no-aliases"
		}
		check := d.planSkip(id, status, doctorQueryCategory(sk), root, fix, sk)
		for _, arg := range argFixes {
			arg.check = check
			d.argFixes = append(d.argFixes, arg)
		}
	}
	d.updateArgumentFixes()
	if argsStatus == "ok" {
		d.add("query.arguments", "ok", "selected generated listings have their required arguments", root, "")
	}
	if aliasStatus == "ok" {
		d.add("query.aliases", "ok", "selected provider configurations can be planned", root, "")
	}
	configs := map[string]tf.IdentityConfig{}
	for _, q := range p.Queries {
		configs[q.Ref()] = tf.IdentityConfig{Ref: q.Ref(), Source: q.Source}
	}
	for address, list := range userLists {
		typ := strings.SplitN(address, ".", 3)[1]
		ref := list.ProviderRef(typ)
		if _, ok := configs[ref]; !ok {
			configs[ref] = tf.IdentityConfig{Ref: ref, Source: userListProvider(ref, typ, declared, schemas)}
		}
	}
	for _, ref := range slices.Sorted(maps.Keys(configs)) {
		c := configs[ref]
		id := "auth." + ref
		why := "-check-auth was not requested"
		if o.doctor.offline {
			why = "offline: authentication disabled"
		} else if len(tf.IdentitySources[model.ProviderFamily(c.Source)]) == 0 {
			why = "provider has no supported identity reader"
		} else if o.doctor.checkAuth {
			type identities struct {
				scopes map[string]map[string]string
				failed map[string]error
			}
			result, err := withSource(ctx, o, func(ctx context.Context) (identities, error) {
				s, f, e := r.Identities(ctx, p, []tf.IdentityConfig{c})
				return identities{s, f}, e
			})
			if err != nil || len(result.failed) > 0 {
				malformed := doctorResponseError(err)
				for _, failure := range result.failed {
					malformed = malformed || doctorResponseError(failure)
				}
				if malformed {
					d.incomplete = true
					d.add(id, "failed", "could not decode identity plan", root, "Check Terraform and provider output, then retry")
				} else {
					d.add(id, "failed", "identity read failed", root, "Check provider credentials and connectivity")
				}
			} else {
				values := []string{}
				for _, k := range slices.Sorted(maps.Keys(result.scopes[ref])) {
					values = append(values, k+"="+result.scopes[ref][k])
				}
				d.add(id, "ok", strings.Join(values, ", "), root, "")
			}
			continue
		}
		d.add(id, "skipped", why, root, "")
	}
	if len(configs) == 0 {
		d.add("auth.providers", "skipped", "no provider configurations selected", root, "")
	}
	return true
}

func doctorResponseError(err error) bool {
	var response *model.ResponseError
	return errors.As(err, &response)
}

func doctorAutoArg(source, name string) bool {
	family := model.ProviderFamily(source)
	_, ok := tf.AutoSources[family[strings.LastIndex(family, "/")+1:]]
	return ok && (name == "region" || name == "zone")
}

func doctorStringArg(schemas *tf.Schemas, source, typ, name string) bool {
	found := false
	for addr, ps := range schemas.ProviderSchemas {
		if source != "" && addr != source {
			continue
		}
		ls, ok := ps.ListResourceSchemas[typ]
		if !ok {
			continue
		}
		if at, ok := ls.Block.Attributes[name]; !ok || string(at.Type) != `"string"` {
			return false
		}
		found = true
	}
	return found
}
