package vm

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/v8bench"
)

// TestRegNativeCaches runs the micro-benchmarks, a little of each test's
// work, as register code by the native handlers and by the Go table's
// alone, and checks that the inline caches the JIT reads -- the property
// sites, the global sites' epochs, the call sites' kinds and callees --
// end up the same (RegCacheDigest): the native fast paths and the Go
// handlers they leave to fill them alike. Where the build has no native
// handlers both runs are the Go table's.
func TestRegNativeCaches(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the micro-benchmarks twice")
	}
	tests, work, err := v8bench.Corpus("micro", "")
	if err != nil {
		t.Fatal(err)
	}
	var prog strings.Builder
	prog.WriteString(`if (typeof console === "undefined") globalThis.console = { log: function () {} };
if (typeof performance === "undefined") globalThis.performance = { now: function () { return 0 } };
var global_res;
`)
	prog.WriteString(tests)
	for _, w := range work {
		fmt.Fprintf(&prog, "\n;try { %s(%d) } catch (e) {}", w.Name, max(w.N/4096, 1))
	}
	text := prog.String()
	digest := func(native bool) string {
		r := newTestRuntime(t, nativeConfig(native))
		defer r.Close()
		if _, err := r.Run(compileForTest(t, text)); err != nil {
			t.Fatal(err)
		}
		return RegCacheDigest(r)
	}
	goTable, native := digest(false), digest(true)
	if n := strings.Count(goTable, "\n"); n < 1000 {
		t.Fatalf("%d lines of caches: too few to tell", n)
	}
	if native != goTable {
		a, b := strings.Split(goTable, "\n"), strings.Split(native, "\n")
		for i := 0; i < len(a) && i < len(b); i++ {
			if a[i] != b[i] {
				t.Fatalf("caches differ at line %d: the Go table's %q, native %q", i, a[i], b[i])
			}
		}
		t.Fatalf("caches differ: %d lines by the Go table, %d natively", len(a), len(b))
	}
}
