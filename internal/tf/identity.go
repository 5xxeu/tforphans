package tf

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/5xxeu/tforphans/internal/model"
)

type IdentityRead struct {
	DataSource string
	Attrs      map[string][]string
}

var IdentitySources = map[string][]IdentityRead{
	"hashicorp/aws": {
		{DataSource: "aws_caller_identity", Attrs: map[string][]string{"account_id": {"account_id"}}},
		{DataSource: "aws_region", Attrs: map[string][]string{"region": {"region", "name"}}},
	},
	"hashicorp/google":  {{DataSource: "google_client_config", Attrs: map[string][]string{"project": {"project"}, "region": {"region"}, "zone": {"zone"}}}},
	"hashicorp/azurerm": {{DataSource: "azurerm_client_config", Attrs: map[string][]string{"subscription_id": {"subscription_id"}, "tenant_id": {"tenant_id"}}}},
}

type IdentityConfig struct{ Ref, Source string }

func identityHCL(configs []IdentityConfig) ([]byte, []string) {
	var b strings.Builder
	var targets []string
	for n, c := range configs {
		for i, read := range IdentitySources[model.ProviderFamily(c.Source)] {
			label := fmt.Sprintf("%sid_%d_%d", LabelPrefix, n, i)
			fmt.Fprintf(&b, "data %q %q {\n  provider = %s\n}\n", read.DataSource, label, c.Ref)
			targets = append(targets, "data."+read.DataSource+"."+label)
		}
	}
	return []byte(b.String()), targets
}

func parseIdentities(data []byte, configs []IdentityConfig) (map[string]map[string]string, map[string]error) {
	var plan struct {
		PriorState struct {
			Values struct {
				RootModule struct {
					Resources []struct {
						Address string         `json:"address"`
						Mode    string         `json:"mode"`
						Values  map[string]any `json:"values"`
					} `json:"resources"`
				} `json:"root_module"`
			} `json:"values"`
		} `json:"prior_state"`
	}
	scopes, failed := map[string]map[string]string{}, map[string]error{}
	if err := model.DecodeJSON(data, &plan); err != nil {
		for _, c := range configs {
			failed[c.Ref] = errors.New("could not decode identity plan")
		}
		return scopes, failed
	}
	values := map[string]map[string]any{}
	for _, r := range plan.PriorState.Values.RootModule.Resources {
		if r.Mode == "data" {
			values[r.Address] = r.Values
		}
	}
	for n, c := range configs {
		scope := map[string]string{}
		for i, read := range IdentitySources[model.ProviderFamily(c.Source)] {
			address := fmt.Sprintf("data.%s.%sid_%d_%d", read.DataSource, LabelPrefix, n, i)
			attrs := values[address]
			keys := make([]string, 0, len(read.Attrs))
			for key := range read.Attrs {
				keys = append(keys, key)
			}
			slices.Sort(keys)
			for _, key := range keys {
				found := false
				for _, attr := range read.Attrs[key] {
					if value, ok := attrs[attr].(string); ok {
						scope[key], found = value, true
						break
					}
				}
				if !found {
					failed[c.Ref] = fmt.Errorf("identity data source %s has no string attribute for %s", read.DataSource, key)
				}
			}
		}
		if failed[c.Ref] == nil {
			scopes[c.Ref] = scope
		}
	}
	return scopes, failed
}

// Identities reads only the attributes that confirm where each configuration looks.
func (r Runner) Identities(ctx context.Context, p Plan, configs []IdentityConfig) (map[string]map[string]string, map[string]error, error) {
	scopes, failed := map[string]map[string]string{}, map[string]error{}
	selected := map[string]IdentityConfig{}
	for _, c := range configs {
		if len(IdentitySources[model.ProviderFamily(c.Source)]) > 0 {
			selected[c.Ref] = c
		}
	}
	configs = nil
	for _, c := range selected {
		configs = append(configs, c)
	}
	slices.SortFunc(configs, func(a, b IdentityConfig) int { return strings.Compare(a.Ref, b.Ref) })
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	if len(configs) == 0 {
		return scopes, failed, nil
	}
	if err := RemoveStale(r.Dir); err != nil {
		return nil, nil, err
	}
	if providers := p.ProvidersHCL(); providers != nil {
		path := filepath.Join(r.Dir, tempName(tempProviderSuffix))
		if err := writeTemp(path, providers); err != nil {
			return nil, nil, err
		}
		defer os.Remove(path)
	}
	dir, err := os.MkdirTemp("", "tforphans-identity-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(r.Dir, tempName(tempIdentitySuffix))
	// A failed plan is isolated by retrying each configuration separately.
	planFailed := false
	read := func(cs []IdentityConfig) (map[string]map[string]string, map[string]error, error) {
		hcl, targets := identityHCL(cs)
		if err := writeTemp(path, hcl); err != nil {
			return nil, nil, err
		}
		defer os.Remove(path)
		planFile := filepath.Join(dir, "identity.tfplan")
		args := []string{"plan", "-refresh=false", "-lock=false", "-input=false", "-no-color"}
		for _, target := range targets {
			args = append(args, "-target="+target)
		}
		args = append(args, "-out="+planFile)
		_, stderr, runErr := r.run(ctx, args...)
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		planFailed = runErr != nil
		var out []byte
		if runErr != nil {
			runErr = cmdError([]string{"plan"}, stderr, runErr)
		} else {
			args = []string{"show", "-json", planFile}
			out, stderr, runErr = r.runLimited(ctx, r.stateSize(), args...)
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			if runErr != nil {
				runErr = cmdError([]string{"show", "-json"}, stderr, runErr)
			}
		}
		if runErr != nil {
			fs := map[string]error{}
			for _, c := range cs {
				fs[c.Ref] = runErr
			}
			return nil, fs, nil
		}
		ss, fs := parseIdentities(out, cs)
		return ss, fs, nil
	}
	scopes, failed, err = read(configs)
	if err != nil {
		return nil, nil, err
	}
	if planFailed && len(configs) > 1 {
		scopes, failed = map[string]map[string]string{}, map[string]error{}
		for _, c := range configs {
			ss, fs, err := read([]IdentityConfig{c})
			if err != nil {
				return nil, nil, err
			}
			for ref, s := range ss {
				scopes[ref] = s
			}
			for ref, e := range fs {
				failed[ref] = e
			}
		}
	}
	return scopes, failed, nil
}

// SelectedWorkspace returns the workspace selected for an unisolated root.
func SelectedWorkspace(dir string) string {
	if workspace := os.Getenv("TF_WORKSPACE"); workspace != "" {
		return workspace
	}
	if data, err := os.ReadFile(filepath.Join(DataDir(dir), "environment")); err == nil {
		if workspace := strings.TrimSpace(string(data)); workspace != "" {
			return workspace
		}
	}
	return "default"
}
