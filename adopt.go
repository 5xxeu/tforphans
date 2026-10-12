package main

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/5xxeu/tforphans/internal/model"
	"github.com/5xxeu/tforphans/internal/report"
	"github.com/5xxeu/tforphans/internal/tf"
)

// hasImport reports whether an import block of the configuration already
// targets r: same type, the same provider configuration, and the same
// literal id or identity. An object listed by a root module only counts
// blocks of that root; one found by -scan or a plugin has no root, and a
// block of any root with its provider configuration counts.
func hasImport(r model.Resource, imports []tf.Import, roots map[string]bool) bool {
	for _, im := range imports {
		if im.Type != r.Type || roots[r.Source] && im.Dir != r.Source {
			continue
		}
		if providerConfig(im.Provider, im.Type) != providerConfig(r.Provider, r.Type) {
			continue
		}
		if im.ID != "" && im.ID == r.ImportID {
			return true
		}
		if im.Identity != nil && r.ImportID == "" && identityEqual(r.Identity, im.Identity) {
			return true
		}
	}
	return false
}

// providerConfig names the provider configuration of a reference such as
// vault.other; none means the default configuration of the type's prefix.
func providerConfig(ref, typ string) string {
	if ref == "" {
		ref, _, _ = strings.Cut(typ, "_")
	}
	return ref
}

func identityEqual(live map[string]any, literal map[string]string) bool {
	n := 0
	for k, v := range live {
		c, ok := model.Canonical(v)
		if !ok {
			continue
		}
		if literal[k] != c {
			return false
		}
		n++
	}
	return n > 0 && n == len(literal)
}

// adopt writes import blocks for the orphans to stdout or to a new file.
// Objects that cannot be written as an import block are warnings.
func adopt(o *options, orphans []model.Resource, reserved map[string]bool, sum *report.Summary, stdout, stderr io.Writer) error {
	if len(orphans) == 0 {
		fmt.Fprintln(stderr, "Nothing to adopt.")
		return nil
	}
	skipped := func(n int) {
		if n < len(orphans) {
			sum.Warnings = append(sum.Warnings, fmt.Sprintf("%d objects have no valid resource type or provider name; no import blocks written for them", len(orphans)-n))
		}
	}
	// Terraform 1.16 lifted this restriction; the hint goes to stderr,
	// so it also reaches people who redirect stdout to a file.
	providerHint := func() {
		if slices.ContainsFunc(orphans, func(r model.Resource) bool { return r.Provider != "" }) {
			fmt.Fprintln(stderr, "With Terraform before 1.16, move the provider line of each import block into its generated resource before applying: older versions generate resources without it, and accept provider in an import block only while its configuration is still to be generated.")
		}
	}
	head := adoptHeader(sum)
	if o.out == "" {
		if _, err := io.WriteString(stdout, head); err != nil {
			return err
		}
		n, err := report.Import(stdout, orphans, reserved)
		skipped(n)
		if err == nil {
			providerHint()
		}
		return err
	}
	f, err := os.OpenFile(o.out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = io.WriteString(f, head)
	n := 0
	if err == nil {
		n, err = report.Import(f, orphans, reserved)
	}
	if err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	skipped(n)
	fmt.Fprintf(stderr, "Wrote %d import blocks to %s. Next, in the root module:\n  terraform plan -generate-config-out=generated.tf\n", n, o.out)
	providerHint()
	return nil
}

// adoptHeader is the comment above the import blocks: what they were
// checked against, and what is not among them.
func adoptHeader(sum *report.Summary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Objects that none of the %d states read manages.\n", sum.States)
	if sum.Uncertain > 0 {
		fmt.Fprintf(&b, "# Not included: %d uncertain objects (matched to state by ID only).\n", sum.Uncertain)
	}
	var gaps []string
	for _, c := range sum.Coverage {
		if c.Status == report.Incomplete && !slices.Contains(gaps, c.Type) {
			gaps = append(gaps, c.Type)
		}
	}
	switch {
	case len(gaps) > 0:
		fmt.Fprintf(&b, "# Coverage incomplete: not fully listed: %s.\n", report.Clean(strings.Join(gaps, ", ")))
	case !sum.Complete:
		b.WriteString("# Coverage incomplete (see the warnings of the run).\n")
	}
	b.WriteString("\n")
	return b.String()
}
