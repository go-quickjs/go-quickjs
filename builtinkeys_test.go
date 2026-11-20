package quickjs_test

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// builtinKeysApart are the keys of a built-in the engine has and Node 26
// has not, or Node has and the engine has not, by the object's path in
// testdata/builtin_keys.js: proposals Node has not shipped, and V8's own
// stack-trace API. Each is where the engine's definition makes it, after
// the keys Node has; any other key either has alone fails the test.
var builtinKeysApart = map[string]struct{ ours, node []string }{
	// The await-dictionary proposal.
	"Promise": {ours: []string{"allKeyed", "allSettledKeyed"}},
	// V8's Error.prepareStackTrace, which the engine does not call.
	"Error": {node: []string{"prepareStackTrace"}},
	// Math.sumPrecise.
	"Math": {ours: []string{"sumPrecise"}},
	// Intl.Locale.prototype.variants (ECMA-402 2026).
	"Intl.Locale.prototype": {ours: []string{"variants"}},
	// Immutable ArrayBuffers.
	"ArrayBuffer.prototype": {ours: []string{"immutable", "transferToImmutable", "sliceToImmutable"}},
	// Joint iteration, iterator chunking, includes and join.
	"Iterator":           {ours: []string{"zip", "zipKeyed"}},
	"Iterator.prototype": {ours: []string{"chunks", "windows", "includes", "join"}},
}

// TestBuiltinKeyOrder lists the own keys of every built-in constructor,
// prototype and namespace (testdata/builtin_keys.js) and compares each with
// Node's, testdata/builtin_keys.golden, in standards mode and under
// WithNodeQuirks alike: the standard leaves the order of a built-in's keys
// to the implementation, and V8's is the one programs and their snapshots
// meet. The keys apart (builtinKeysApart) are left out of the comparison of
// order; standards mode's Error.prototype.stack, an accessor V8 makes each
// error's own property, is one more of the engine's there.
func TestBuiltinKeyOrder(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("testdata/builtin_keys.js")
	if err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile("testdata/builtin_keys.golden")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string][]string
	if err := json.Unmarshal(golden, &want); err != nil {
		t.Fatal(err)
	}
	for _, quirks := range []bool{false, true} {
		var opts []quickjs.Option
		if quirks {
			opts = append(opts, quickjs.WithNodeQuirks())
		}
		rt := quickjs.New(opts...)
		v, err := rt.Eval(string(src))
		if err != nil {
			rt.Close()
			t.Fatalf("quirks=%v: %v", quirks, err)
		}
		var got map[string][]string
		if err := json.Unmarshal([]byte(v.String()), &got); err != nil {
			rt.Close()
			t.Fatalf("quirks=%v: %v", quirks, err)
		}
		rt.Close()
		for path, node := range want {
			ours, ok := got[path]
			if !ok {
				t.Errorf("quirks=%v: %s not listed", quirks, path)
				continue
			}
			apart := builtinKeysApart[path]
			oursApart := apart.ours
			if path == "Error.prototype" && !quirks {
				oursApart = []string{"stack"}
			}
			var onlyOurs, onlyNode []string
			for _, k := range ours {
				if !slices.Contains(node, k) {
					onlyOurs = append(onlyOurs, k)
				}
			}
			for _, k := range node {
				if !slices.Contains(ours, k) {
					onlyNode = append(onlyNode, k)
				}
			}
			if !slices.Equal(onlyOurs, oursApart) || !slices.Equal(onlyNode, apart.node) {
				t.Errorf("quirks=%v: %s: keys Node has not %v, keys only Node has %v; want %v and %v",
					quirks, path, onlyOurs, onlyNode, oursApart, apart.node)
				continue
			}
			common := slices.DeleteFunc(slices.Clone(ours), func(k string) bool { return slices.Contains(onlyOurs, k) })
			nodeCommon := slices.DeleteFunc(slices.Clone(node), func(k string) bool { return slices.Contains(onlyNode, k) })
			if !slices.Equal(common, nodeCommon) {
				t.Errorf("quirks=%v: %s's keys\n got %s\nwant %s", quirks, path, strings.Join(common, ","), strings.Join(nodeCommon, ","))
			}
		}
		for path := range got {
			if _, ok := want[path]; !ok {
				t.Errorf("quirks=%v: %s listed, not in Node's", quirks, path)
			}
		}
	}
}
