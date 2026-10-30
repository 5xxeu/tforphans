package report

import (
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseExpect(t *testing.T) {
	got, err := ParseExpect([]string{"account_id=1,2 region=a", "region=a account_id=2", "project=p,,p"})
	if err != nil {
		t.Fatal(err)
	}
	want := []map[string]string{
		{"account_id": "1", "region": "a"},
		{"account_id": "2", "region": "a"},
		{"project": "p"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	for _, bad := range []string{"", "  ", "account_id", "=x", "Account=x", "a-b=x", "project=", "project=,", "project=a project=b"} {
		if _, err := ParseExpect([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	// A value may hold "=": only the first one separates the key.
	if got, err := ParseExpect([]string{"tag=a=b"}); err != nil || got[0]["tag"] != "a=b" {
		t.Errorf("value with =: %v %v", got, err)
	}
	if _, err := ParseExpect([]string{"a=" + strings.Join(numbered(101), ",") + " b=" + strings.Join(numbered(100), ",")}); err == nil {
		t.Error("product above the limit accepted")
	}
	// Repeated entries count once, also near the limit.
	half := "project=" + strings.Join(numbered(6000), ",")
	if got, err := ParseExpect([]string{half, half}); err != nil || len(got) != 6000 {
		t.Errorf("repeated entries: %d %v", len(got), err)
	}
	if _, err := ParseExpect([]string{half, "zone=" + strings.Join(numbered(5000), ",")}); err == nil {
		t.Error("unique scopes above the limit accepted")
	}
	keys := make([]string, maxExpectKeys+1)
	for i := range keys {
		keys[i] = "k" + strconv.Itoa(i) + "=v"
	}
	if _, err := ParseExpect([]string{strings.Join(keys, " ")}); err == nil {
		t.Error("too many keys accepted")
	}
	// Many values (a config file is read before -timeout) fail fast.
	start := time.Now()
	if _, err := ParseExpect([]string{"a=" + strings.Repeat("x,", 1<<19) + strings.Join(numbered(MaxExpected+1), ",")}); err == nil || time.Since(start) > 5*time.Second {
		t.Errorf("many values: %v after %s", err, time.Since(start))
	}
}

func numbered(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "v" + strconv.Itoa(i)
	}
	return out
}

func TestCheckExpected(t *testing.T) {
	listings := []Listing{
		{Type: "aws_instance", Complete: false, Scope: map[string]string{"account_id": "1", "region": "a"}},
		{Type: "aws_vpc", Complete: true, Scope: map[string]string{"account_id": "1", "region": "a"}},
		{Type: "aws_instance", Complete: false, Scope: map[string]string{"account_id": "2", "region": "a"}},
		{Type: "proxmox_vm", Complete: true},
	}
	want := []map[string]string{
		{"account_id": "1"},
		{"account_id": "1", "region": "a"},
		{"account_id": "2", "region": "a"},
		{"account_id": "3"},
		{"account_id": "1", "regoin": "a"},
	}
	got, unknown := CheckExpected(want, listings)
	status := []string{}
	counts := []int{}
	for _, e := range got {
		status = append(status, e.Status)
		counts = append(counts, e.Listings)
	}
	if !reflect.DeepEqual(status, []string{ExpectConfirmed, ExpectConfirmed, ExpectIncomplete, ExpectMissing, ExpectMissing}) ||
		!reflect.DeepEqual(counts, []int{2, 2, 1, 0, 0}) {
		t.Fatalf("status %v, listings %v", status, counts)
	}
	if got[2].ReasonCode != "expected_scope_incomplete" || got[3].ReasonCode != "expected_scope_missing" || got[0].ReasonCode != "" {
		t.Fatalf("codes: %+v", got)
	}
	if !reflect.DeepEqual(unknown, []string{"regoin"}) {
		t.Fatalf("unknown keys %v", unknown)
	}
	data, _ := json.Marshal(got[0])
	if string(data) != `{"scope":{"account_id":"1"},"status":"confirmed","listings":2}` {
		t.Fatalf("confirmed JSON: %s", data)
	}
	data, _ = json.Marshal(got[3])
	if string(data) != `{"scope":{"account_id":"3"},"status":"missing","listings":0,"reason_code":"expected_scope_missing"}` {
		t.Fatalf("missing JSON: %s", data)
	}
	docs, err := os.ReadFile("../../docs/reasons.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"expected_scope_missing", "expected_scope_incomplete"} {
		if !strings.Contains(string(docs), `id="`+code+`"`) {
			t.Errorf("undocumented code: %s", code)
		}
	}
}
