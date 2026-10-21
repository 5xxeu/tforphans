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
