package v8bench

import (
	"encoding/json"
	"fmt"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

func TestStringSwitchMicrobenchmarks(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.Eval("var global_res;\n" + microbenchExtra); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		sum  int
	}{
		{"string_switch_first", 9},
		{"string_switch_last", 144},
		{"string_switch_miss", 0},
		{"string_switch_mixed", 51},
		{"string_switch_built", 51},
		{"string_switch_small", 13},
		{"string_switch_large", 207},
		{"string_switch_prefix", 87},
		{"string_switch_long", 70},
		{"string_switch_typeof", 25},
		{"string_switch_nonstring", 0},
		{"string_switch_nonliteral", 46},
		{"number_switch", 53},
		{"typeof_test", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := rt.Eval(fmt.Sprintf("%s(9) + ':' + global_res", tc.name))
			want := fmt.Sprintf("9:%d", tc.sum)
			if err != nil || v.String() != want {
				t.Fatalf("got %v, %v; want %q", v, err, want)
			}
		})
	}
}

func TestAllocationMicrobenchmarks(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.Eval("var global_res;\n" + microbenchExtra); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ src, want string }{
		{`object_literal_fixed(9) + ':' + JSON.stringify(global_res)`, `9:{"a":8,"b":9,"c":"key","d":true,"e":null,"f":8,"g":false,"h":"end"}`},
		{`array_push_large(1) + ':' + global_res`, "8192:16383"},
		{`upvalue_string_append(1025) + ':' + global_res`, "2048:12282"},
		{`escape_accumulate(3) + ':' + global_res`, "300:15876"},
		{`escape_accumulate_long(3) + ':' + global_res`, "300:15876"},
		{`escape_join(9) + ':' + global_res`, "9:36"},
		{`escape_rare(1025) + ':' + global_res`, "1025:529408"},
	} {
		v, err := rt.Eval(tc.src)
		if err != nil || v.String() != tc.want {
			t.Fatalf("%s = %v, %v; want %q", tc.src, v, err, tc.want)
		}
	}
}

func TestHTMLEscapeMicrobenchmarks(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.Eval("var global_res;\n" + microbenchExtra); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ src, want string }{
		{`extra_html_escape("")`, ""},
		{`extra_html_escape("plain text")`, "plain text"},
		{`extra_html_escape("Tom & Jerry")`, "Tom &amp; Jerry"},
		{`extra_html_escape('<a title="x">\'sale\'</a>')`, "&lt;a title=&quot;x&quot;&gt;&#x27;sale&#x27;&lt;/a&gt;"},
		{`extra_html_escape('"\'&<>')`, "&quot;&#x27;&amp;&lt;&gt;"},
		{`extra_html_escape("<tail")`, "&lt;tail"},
		{`extra_html_escape("head>")`, "head&gt;"},
		{`extra_html_escape("\ud83d\ude00 & \u00e9")`, "\U0001f600 &amp; \u00e9"},
		{`extra_html_escape(42)`, "42"},
		{`extra_html_escape(true)`, "true"},
		{`extra_html_escape(42n)`, "42"},
		{`extra_html_escape(null)`, "null"},
		{`extra_html_escape(undefined)`, "undefined"},
		{`html_escape_plain(9) + ':' + global_res`, "9:91"},
		{`html_escape_entities(9) + ':' + global_res`, "9:339"},
		{`html_escape_mixed(9) + ':' + global_res`, "9:107"},
	} {
		t.Run(tc.src, func(t *testing.T) {
			v, err := rt.Eval(tc.src)
			if err != nil || v.String() != tc.want {
				t.Fatalf("got %v, %v; want %q", v, err, tc.want)
			}
		})
	}
}

func TestHasOwnAndPushMicrobenchmarks(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.Eval("var global_res;\n" + microbenchExtra); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ src, want string }{
		{`has_own_call_hit(9) + ':' + global_res`, "9:9"},
		{`has_own_call_miss(9) + ':' + global_res`, "9:0"},
		{`has_own_call_for_in(9) + ':' + global_res`, "36:36"},
		{`has_own_method_hit(9) + ':' + global_res`, "9:9"},
	} {
		t.Run(tc.src, func(t *testing.T) {
			v, err := rt.Eval(tc.src)
			if err != nil || v.String() != tc.want {
				t.Fatalf("got %v, %v; want %q", v, err, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		name  string
		parts []string
	}{
		{"array_push_args3", []string{" ", "disabled", `=""`}},
		{"array_push_args3_single", []string{" ", "disabled", `=""`}},
		{"array_push_args5", []string{" ", "data-id", `="`, "42", `"`}},
		{"array_push_args5_single", []string{" ", "data-id", `="`, "42", `"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var parts []string
			for i := 0; i < 1024; i++ {
				parts = append(parts, tc.parts...)
			}
			encoded, err := json.Marshal(parts)
			if err != nil {
				t.Fatal(err)
			}
			src := tc.name + "(2) + ':' + global_res.length + ':' + JSON.stringify(global_res)"
			want := fmt.Sprintf("2048:%d:%s", len(parts), encoded)
			v, err := rt.Eval(src)
			if err != nil || v.String() != want {
				t.Fatalf("got %v, %v; want %q", v, err, want)
			}
		})
	}
}

// Objects made on prototypes made and dropped with them, on one they share,
// classes and subclasses made by each call, scopes as objects, and one
// constructor for alternating new.targets: each test's sum, as Node gives it.
func TestPrototypeMicrobenchmarks(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.Eval("var global_res;\n" + microbenchExtra); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		sum  int
	}{
		{"object_create_fresh", 45},
		{"object_create_shared", 45},
		{"proto_literal_fresh", 45},
		{"set_proto_fresh", 45},
		{"class_factory", 36},
		{"subclass_fresh", 45},
		{"scope_chain", 81},
		{"construct_new_target", 45},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := rt.Eval(fmt.Sprintf("%s(9) + ':' + global_res", tc.name))
			want := fmt.Sprintf("9:%d", tc.sum)
			if err != nil || v.String() != want {
				t.Fatalf("got %v, %v; want %q", v, err, want)
			}
		})
	}
}
