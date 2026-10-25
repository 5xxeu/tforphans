package cli

import (
	"flag"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestDoctorNextFlagValues(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("-format", []byte("quiet: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                             string
		args, want, roots, states, fixes []string
	}{
		{"state value", []string{"-state", "-offline", "ROOT"}, []string{"tforphans", "-state", "-offline", "--", "ROOT"}, []string{"ROOT"}, []string{"-offline"}, nil},
		{"config value", []string{"-config", "-format"}, []string{"tforphans", "-config", "-format"}, []string{"."}, nil, nil},
		{"attached format", []string{"-format=json"}, []string{"tforphans"}, []string{"."}, nil, nil},
		{"false offline", []string{"-offline=false"}, []string{"tforphans"}, []string{"."}, nil, nil},
		{"end flags", []string{"--", "-offline", "-format=json", "ROOT"}, []string{"tforphans", "--", "-offline", "-format=json", "ROOT"}, []string{"-offline", "-format=json", "ROOT"}, nil, nil},
		{"flags after roots", []string{"ROOT", "--offline", "-state", "-check-auth", "--format=json"}, []string{"tforphans", "-state", "-check-auth", "--", "ROOT"}, []string{"ROOT"}, []string{"-check-auth"}, nil},
		{"format after root", []string{"ROOT", "-format", "json", "-state", "-format"}, []string{"tforphans", "-state", "-format", "--", "ROOT"}, []string{"ROOT"}, []string{"-format"}, nil},
		{"boolean assignment", []string{"-quiet=false", "--current-workspace=false", "--offline=false"}, []string{"tforphans", "-quiet=false", "--current-workspace=false"}, []string{"."}, nil, nil},
		{"end marker as value", []string{"-state", "--", "ROOT", "-offline"}, []string{"tforphans", "-state", "--", "--", "ROOT"}, []string{"ROOT"}, []string{"--"}, nil},
		{"fixes before end flags", []string{"ROOT", "--", "-format"}, []string{"tforphans", "-arg", "region=...", "--", "ROOT", "-format"}, []string{"ROOT", "-format"}, nil, []string{"-arg", "region=..."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, err := parseFlags(append([]string{"doctor"}, tc.args...), io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			got := doctorNext(o.doctorArgs, tc.fixes)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("%q want %q", got, tc.want)
			}
			if !reflect.DeepEqual([]string(o.roots), tc.roots) || !reflect.DeepEqual([]string(o.states), tc.states) {
				t.Fatalf("roots=%q states=%q", o.roots, o.states)
			}
			// The audit must see the same roots and state values in the proposed command.
			audit, err := parseFlags(got[1:], io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(audit.roots, o.roots) || !reflect.DeepEqual(audit.states, o.states) {
				t.Fatalf("audit roots=%q states=%q, doctor roots=%q states=%q", audit.roots, audit.states, o.roots, o.states)
			}
		})
	}
}

func TestCommandArgsFlagMetadata(t *testing.T) {
	o, err := parseFlags([]string{"doctor", "-no-config"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	o.doctorFlagSet.VisitAll(func(f *flag.Flag) {
		b, ok := f.Value.(interface{ IsBoolFlag() bool })
		boolean := ok && b.IsBoolFlag()
		args := []string{"-" + f.Name, "-offline", "ROOT"}
		parsed := commandArgs(o.doctorFlagSet, args)
		if parsed[0].separate == boolean {
			t.Fatalf("-%s value consumption: %+v", f.Name, parsed)
		}
		if !boolean && (parsed[0].value != "-offline" || len(parsed) != 2) {
			t.Fatalf("-%s: %+v", f.Name, parsed)
		}
	})
}

func TestDoctorNextValueRedaction(t *testing.T) {
	args := []string{"ROOT", "-state", "https://user:password@host/state?filter=a,b&token=SECRET", "-arg", "api_key=SECRET", "-plugin", "echo SECRET", "--", "-offline"}
	got := doctorNextArgs(t, args, nil)
	joined := strings.Join(got, " ")
	if strings.Contains(joined, "SECRET") || strings.Contains(joined, "password") {
		t.Fatal(got)
	}
	if got[len(got)-1] != "-offline" {
		t.Fatal(got)
	}
	if !strings.Contains(joined, "https://host/state?REDACTED") || !strings.Contains(joined, "api_key=<redacted>") || !strings.Contains(joined, "<plugin command>") {
		t.Fatal(got)
	}
}
