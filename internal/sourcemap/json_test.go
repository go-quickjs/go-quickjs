package sourcemap

import (
	"reflect"
	"testing"
)

// TestJSON pins the JSON a map is read with: every kind of value, a
// string's escapes -- a surrogate pair as one character, a lone surrogate
// as the replacement character -- and what is not JSON.
func TestJSON(t *testing.T) {
	for _, c := range []struct {
		in   string
		want any
	}{
		{`{"a":[1,-2.5e1,true,false,null],"b":{"c":""}}`,
			map[string]any{"a": []any{1.0, -25.0, true, false, nil}, "b": map[string]any{"c": ""}}},
		{` [ ] `, []any{}},
		{`"x\"\\\/\b\f\n\r\t\u0041\u00e9\ud83d\ude00"`, "x\"\\/\b\f\n\r\tA\u00e9\U0001f600"},
		{`"\ud83d"`, "\uFFFD"},
	} {
		got, err := parseJSON([]byte(c.in))
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %#v, %v", c.in, got, err)
		}
	}
	for _, bad := range []string{``, `{`, `[1,]`, `{"a" 1}`, `"\x"`, "\"a\nb\"", `tru`, `1 2`, `{"a":1,}`} {
		if _, err := parseJSON([]byte(bad)); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}
