package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"slices"
	"testing"
)

// The Junos plugin against a saved configuration and a state of the same
// device: names only, all uncertain until -plugin-cluster-states, then the
// three objects in state are managed.
func TestJunosPlugin(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not in PATH")
	}
	plugin := "python3 examples/plugins/junos.py testdata/junos/config.json"
	report := func(args ...string) (int, map[string]any, string) {
		var out, errb bytes.Buffer
		code := run(context.Background(), append([]string{"-no-query", "-state", "testdata/junos/terraform.tfstate", "-plugin", plugin, "-format", "json"}, args...), &out, &errb)
		var rep map[string]any
		if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
			t.Fatalf("exit %d: %v\n%s\n%s", code, err, out.String(), errb.String())
		}
		if bytes.Contains(out.Bytes(), []byte("secret")) || bytes.Contains(errb.Bytes(), []byte("secret")) {
			t.Error("configuration text in the output")
		}
		return code, rep["summary"].(map[string]any), errb.String()
	}
	code, sum, stderr := report()
	if code != exitOrphans || sum["live"] != 17.0 || sum["managed"] != 0.0 || sum["uncertain"] != 3.0 || sum["orphans"] != 14.0 {
		t.Errorf("without -plugin-cluster-states: exit %d %v\n%s", code, sum, stderr)
	}
	code, sum, stderr = report("-plugin-cluster-states")
	if code != exitOrphans || sum["managed"] != 3.0 || sum["uncertain"] != 0.0 || sum["orphans"] != 14.0 {
		t.Errorf("with -plugin-cluster-states: exit %d %v\n%s", code, sum, stderr)
	}
	// A device that cannot be read is a warning: the run goes on, coverage
	// is incomplete.
	var out, errb bytes.Buffer
	code = run(context.Background(), []string{"-no-query", "-state", "testdata/junos/terraform.tfstate", "-plugin", "python3 examples/plugins/junos.py testdata/junos/missing.json", "-strict"}, &out, &errb)
	if code != exitError || !bytes.Contains(errb.Bytes(), []byte("cannot read the configuration")) || !bytes.Contains(errb.Bytes(), []byte("coverage: INCOMPLETE")) {
		t.Errorf("unreadable device: exit %d\n%s", code, errb.String())
	}
	var types []string
	for k := range sum["by_type"].(map[string]any) {
		types = append(types, k)
	}
	slices.Sort(types)
	want := []string{"junos_application", "junos_application_set", "junos_bgp_group", "junos_bgp_neighbor", "junos_interface_logical",
		"junos_interface_physical", "junos_policyoptions_policy_statement", "junos_policyoptions_prefix_list", "junos_security_zone"}
	if !slices.Equal(types, want) {
		t.Errorf("types %v", types)
	}
}
