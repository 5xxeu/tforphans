package tf

import (
	"strings"
	"testing"
)

func TestParseArgErrorsHideValues(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"SECRET", "invalid -arg, want [TYPE_GLOB:]name=value[,value...]"},
		{"=SECRET", "invalid -arg, want [TYPE_GLOB:]name=value[,value...]"},
		{"type: =SECRET", "invalid -arg, want [TYPE_GLOB:]name=value[,value...]"},
		{" :api_key=SECRET", "invalid -arg, want [TYPE_GLOB:]name=value[,value...]"},
		{" api_key = , \t ", `invalid -arg "api_key": no values`},
		{" type_* : api_key = , \t ", `invalid -arg "type_*:api_key": no values`},
		{"api_key=SECRET,auto", `invalid -arg "api_key": auto stands for every value and takes no others`},
		{" type_* : api_key =auto,SECRET", `invalid -arg "type_*:api_key": auto stands for every value and takes no others`},
	} {
		t.Run(tc.input, func(t *testing.T) {
			_, err := ParseArg(tc.input)
			if err == nil || err.Error() != tc.want || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
		})
	}
}

// TestCheckArgCombinations: the bound counts every value a block could
// combine, scoped -arg of one name merged, before anything is built.
func TestCheckArgCombinations(t *testing.T) {
	values := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = strings.Repeat("v", i+1)
		}
		return out
	}
	for _, tc := range []struct {
		name string
		args []Arg
		ok   bool
	}{
		{"one arg at the limit", []Arg{{Name: "zone", Values: values(MaxArgCombinations)}}, true},
		{"one arg over", []Arg{{Name: "zone", Values: values(MaxArgCombinations + 1)}}, false},
		{"product", []Arg{{Name: "zone", Values: values(101)}, {Name: "project", Values: values(100)}}, false},
		{"scoped replaces plain", []Arg{{Name: "zone", Values: values(100)}, {Types: "a_*", Name: "zone", Values: values(50)}, {Name: "project", Values: values(100)}}, true},
		{"scoped merge", []Arg{{Types: "a_*", Name: "zone", Values: values(60)}, {Types: "*_b", Name: "zone", Values: values(60)}, {Name: "project", Values: values(100)}}, false},
	} {
		if err := CheckArgCombinations(tc.args); (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}
