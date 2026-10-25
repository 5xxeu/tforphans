package main

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/5xxeu/tforphans/internal/tf"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/zclconf/go-cty/cty"
)

// initRoot is deliberately local metadata only: no provider configuration
// values, backend addresses, credentials, states or resource values.
type initRoot struct {
	Dir, Backend string
	Initialized  bool
	Providers    []initProvider
}
type initProvider struct {
	Source, Version string
	Configs         []string
}

func inspectInitRoot(dir string) (initRoot, error) {
	out := initRoot{Dir: dir, Backend: "local (implicit)", Initialized: (tf.Runner{Dir: dir}).Initialized()}
	configs, err := tf.ProviderSourceConfigs(dir)
	if err != nil {
		return out, fmt.Errorf("%s: Terraform configuration could not be parsed; check it before running init again", dir)
	}
	providers := map[string]initProvider{}
	for source, config := range configs {
		p := initProvider{Source: source}
		if config.Plain || len(config.Aliases) == 0 {
			p.Configs = append(p.Configs, config.LocalNames...)
		}
		for _, alias := range config.Aliases {
			p.Configs = append(p.Configs, alias.Ref())
		}
		slices.Sort(p.Configs)
		providers[source] = p
	}
	lock := filepath.Join(dir, ".terraform.lock.hcl")
	if raw, err := os.ReadFile(lock); err == nil {
		f, diags := hclparse.NewParser().ParseHCL(raw, lock)
		if diags.HasErrors() {
			return out, fmt.Errorf("%s: provider lock file could not be parsed", dir)
		}
		content, _, diags := f.Body.PartialContent(&hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "provider", LabelNames: []string{"source"}}}})
		if diags.HasErrors() {
			return out, fmt.Errorf("%s: invalid provider lock file", dir)
		}
		for _, block := range content.Blocks {
			attrs, diags := block.Body.JustAttributes()
			if diags.HasErrors() || attrs["version"] == nil {
				return out, fmt.Errorf("%s: lock entry has no valid version", dir)
			}
			version, diags := attrs["version"].Expr.Value(nil)
			if diags.HasErrors() || version.IsNull() || !version.IsKnown() || version.Type() != cty.String {
				return out, fmt.Errorf("%s: lock entry has no literal version", dir)
			}
			source := tf.NormalizeSource(block.Labels[0])
			p := providers[source]
			p.Source, p.Version = source, version.AsString()
			providers[source] = p
		}
	} else if !os.IsNotExist(err) {
		return out, fmt.Errorf("%s: provider lock file could not be read", dir)
	}
	for _, source := range slices.Sorted(maps.Keys(providers)) {
		out.Providers = append(out.Providers, providers[source])
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out, err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || tf.IgnoredFile(name) {
			continue
		}
		parser := hclparse.NewParser()
		var f *hcl.File
		var diags hcl.Diagnostics
		switch {
		case strings.HasSuffix(name, ".tf"):
			f, diags = parser.ParseHCLFile(filepath.Join(dir, name))
		case strings.HasSuffix(name, ".tf.json"):
			f, diags = parser.ParseJSONFile(filepath.Join(dir, name))
		default:
			continue
		}
		if diags.HasErrors() {
			return out, fmt.Errorf("%s: Terraform configuration could not be parsed", dir)
		}
		content, _, diags := f.Body.PartialContent(&hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "terraform"}}})
		if diags.HasErrors() {
			return out, fmt.Errorf("%s: invalid terraform block", dir)
		}
		for _, block := range content.Blocks {
			backend, _, diags := block.Body.PartialContent(&hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "backend", LabelNames: []string{"type"}}, {Type: "cloud"}}})
			if diags.HasErrors() {
				return out, fmt.Errorf("%s: invalid backend block", dir)
			}
			for _, b := range backend.Blocks {
				out.Backend = b.Type
				if b.Type == "backend" {
					out.Backend = b.Labels[0]
				}
			}
		}
	}
	return out, nil
}
