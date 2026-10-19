package report

import (
	"bytes"
	"encoding/json"
	"testing"
	"unicode/utf8"

	"github.com/5xxeu/tforphans/internal/plugin"
	"github.com/5xxeu/tforphans/internal/tf"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// FuzzImportHCL feeds plugin output through adopt: whatever a plugin
// prints, the import blocks parse as HCL, nothing in them is a template,
// and every id and identity value comes back exactly (numbers of any size).
func FuzzImportHCL(f *testing.F) {
	for _, s := range []string{
		`{"type":"vault_policy","identity":{"id":"a"}}`,
		`{"type":"x_y","provider":"x.other","import_id":"${file(\"/etc/passwd\")}","identity":{"id":"x"}}` + "\n" + `{"type":"x_y","identity":{"n":12345678901234567890123,"f":-1.5e300}}`,
		`{"type":"x_y","identity":{"k\"}\n%{if}":["a",{"b":null,"c":true}],"e":{}},"display_name":"Тест VM","scope":"\u001b[2J"}`,
		`{"type":"x_y","identity":{"id":"line\r\nbreak\t\\$${x}"},"undeclared_provider":"a\nb"}`,
		`{"type":"x_y","identity":{"a":-0,"b":1e999999,"c":0.000000000000000000000000000001,"d":-12345678901234567890.25}}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		found, _, _, err := plugin.Parse(data, "plugin1")
		if err != nil {
			return
		}
		var out bytes.Buffer
		written, err := Import(&out, found, nil)
		if err != nil {
			t.Fatal(err)
		}
		file, diags := hclsyntax.ParseConfig(out.Bytes(), "imports.tf", hcl.InitialPos)
		if diags.HasErrors() {
			t.Fatalf("%s\n%s", diags, &out)
		}
		blocks := file.Body.(*hclsyntax.Body).Blocks
		if len(blocks) != written || written != len(found) {
			t.Fatalf("%d blocks, %d written, %d found\n%s", len(blocks), written, len(found), &out)
		}
		for i, b := range blocks {
			r := found[i]
			if b.Type != "import" {
				t.Fatalf("block %q", b.Type)
			}
			to, ok := b.Body.Attributes["to"]
			if !ok {
				t.Fatalf("no to\n%s", &out)
			}
			if trav, d := hcl.AbsTraversalForExpr(to.Expr); d.HasErrors() || len(trav) != 2 || trav.RootName() != r.Type {
				t.Fatalf("to %v %s\n%s", trav, d, &out)
			}
			if r.ImportID != "" {
				got := eval(t, b, "id", &out)
				if !got.RawEquals(cty.StringVal(r.ImportID)) {
					t.Fatalf("id %#v, want %q\n%s", got, r.ImportID, &out)
				}
				continue
			}
			got := eval(t, b, "identity", &out)
			for k, v := range r.Identity {
				if v == nil {
					if got.Type().HasAttribute(k) {
						t.Fatalf("null %q written\n%s", k, &out)
					}
					continue
				}
				if !got.Type().HasAttribute(k) || !got.GetAttr(k).RawEquals(ctyValue(t, v)) {
					t.Fatalf("identity %q: %#v, want %#v\n%s", k, got, v, &out)
				}
			}
		}
	})
}

// eval evaluates an attribute without functions or variables: any template
// or reference that slipped through fails here.
func eval(t *testing.T, b *hclsyntax.Block, name string, out *bytes.Buffer) cty.Value {
	t.Helper()
	attr, ok := b.Body.Attributes[name]
	if !ok {
		t.Fatalf("no %s\n%s", name, out)
	}
	v, d := attr.Expr.Value(nil)
	if d.HasErrors() {
		t.Fatalf("%s: %s\n%s", name, d, out)
	}
	return v
}

func ctyValue(t *testing.T, v any) cty.Value {
	switch v := v.(type) {
	case nil:
		return cty.NullVal(cty.DynamicPseudoType)
	case string:
		return cty.StringVal(v)
	case bool:
		return cty.BoolVal(v)
	case json.Number:
		n, err := cty.ParseNumberVal(v.String())
		if err != nil {
			t.Fatalf("number %s: %v", v, err)
		}
		return n
	case []any:
		if len(v) == 0 {
			return cty.EmptyTupleVal
		}
		vals := make([]cty.Value, len(v))
		for i, e := range v {
			vals[i] = ctyValue(t, e)
		}
		return cty.TupleVal(vals)
	case map[string]any:
		if len(v) == 0 {
			return cty.EmptyObjectVal
		}
		vals := map[string]cty.Value{}
		for k, e := range v {
			vals[k] = ctyValue(t, e)
		}
		return cty.ObjectVal(vals)
	}
	t.Fatalf("unexpected %T", v)
	return cty.NilVal
}

// FuzzHCLString: any text quoted by HCLString is read back as the same
// string, never as a template.
func FuzzHCLString(f *testing.F) {
	for _, s := range []string{"", "plain", "${x}", "%{if x}", "$${x}", `a\"b`, "\x00\x1b ", "é"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !utf8.ValidString(s) {
			return // IDs come from JSON, which is valid UTF-8
		}
		src := "x = " + tf.HCLString(s) + "\n"
		file, diags := hclsyntax.ParseConfig([]byte(src), "x.tf", hcl.InitialPos)
		if diags.HasErrors() {
			t.Fatalf("%q: %s", src, diags)
		}
		v, d := file.Body.(*hclsyntax.Body).Attributes["x"].Expr.Value(nil)
		if d.HasErrors() || !v.RawEquals(cty.StringVal(s)) {
			t.Fatalf("%q: %#v %s", src, v, d)
		}
	})
}
