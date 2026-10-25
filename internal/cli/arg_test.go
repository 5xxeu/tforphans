package cli

import (
	"bytes"
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/5xxeu/tforphans/internal/tf"
)

func TestInvalidArgOutput(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, command := range [][]string{nil, {"-v"}, {"-print-query"}, {"doctor"}, {"doctor", "-v"}} {
		for _, source := range []string{"CLI", "automatic config", "explicit config"} {
			t.Run(strings.Join(command, " ")+"/"+source, func(t *testing.T) {
				args := slices.Clone(command)
				if source == "CLI" {
					args = append(args, "-arg", "api_key=SECRET,auto")
				} else {
					if err := os.WriteFile(defaultConfigFile, []byte("arg: ['api_key=SECRET,auto']\n"), 0600); err != nil {
						t.Fatal(err)
					}
					defer os.Remove(defaultConfigFile)
					if source == "explicit config" {
						args = append(args, "-config", defaultConfigFile)
					}
				}
				var out, stderr bytes.Buffer
				if code := run(context.Background(), append(args, "."), &out, &stderr); code != 1 {
					t.Fatalf("exit %d: %s %s", code, &out, &stderr)
				}
				if strings.Contains(stderr.String(), "SECRET") || strings.Contains(out.String(), "SECRET") {
					t.Fatalf("argument value exposed: %s %s", &out, &stderr)
				}
				if !strings.Contains(stderr.String(), `invalid -arg "api_key"`) || out.Len() != 0 {
					t.Fatalf("unexpected output: %s %s", &out, &stderr)
				}
			})
		}
	}
}

func TestPrintQueryRedactsArguments(t *testing.T) {
	bin := doctorFixture(t)
	t.Setenv("DOCTOR_SCHEMA_JSON", doctorTestSchema(t, map[string]map[string][]string{
		"registry.terraform.io/hashicorp/aws": {"aws_thing": {"api_key", "endpoint", "region"}},
	}))
	var out, stderr bytes.Buffer
	args := []string{"-no-config", "-terraform", bin, "-print-query", "-v", "-arg", "api_key=SECRET", "-arg", "endpoint=https://user:SECRET@host/path?token=SECRET", "-arg", "region=eu-north-1", "."}
	if code := run(context.Background(), args, &out, &stderr); code != 0 {
		t.Fatalf("exit %d: %s %s", code, &out, &stderr)
	}
	if strings.Contains(out.String(), "SECRET") || strings.Contains(stderr.String(), "SECRET") || strings.Contains(out.String(), "user:") {
		t.Fatalf("argument credentials exposed: %s %s", &out, &stderr)
	}
	if !strings.Contains(out.String(), "<redacted>") || !strings.Contains(out.String(), "eu-north-1") {
		t.Fatalf("unexpected query: %s", &out)
	}
	plan := tf.Plan{Queries: []tf.Query{{Type: "aws_thing", Provider: "aws", Args: []tf.Arg{{Name: "api_key", Values: []string{"SECRET"}}}}}}
	before := string(plan.HCL(100))
	out.Reset()
	printQueryFiles(&out, ".", plan, 100)
	if string(plan.HCL(100)) != before || strings.Contains(out.String(), "SECRET") {
		t.Fatal("printing must redact without changing the executable plan")
	}
}
