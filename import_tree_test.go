package quickjs_test

import (
	"context"
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// TestImportInTrees pins that import() and import.meta in functions that
// run as trees give what they give in the interpreter: modules loaded by a
// loop, with the referrer each is resolved against, import attributes
// passed on, a specifier whose toString throws rejecting rather than
// throwing, and the module's import.meta read in a loop.
func TestImportInTrees(t *testing.T) {
	defer vm.SetTreeTier(true)
	run := func(tree bool) (string, int64) {
		vm.SetTreeTier(tree)
		rt := quickjs.New()
		defer rt.Close()
		var got []string
		rt.SetModuleLoader(func(specifier, referrer string) (string, string, error) {
			got = append(got, specifier+" from "+referrer)
			if strings.HasSuffix(specifier, ".json") {
				return `{"v": 7}`, "/app/" + specifier, nil
			}
			return `export const name = "` + specifier + `";`, "/app/" + specifier, nil
		})
		before := vm.TreesBuilt()
		ns, err := rt.EvalModule("/app/main.mjs", `
			function load(n) {
				const ps = [];
				for (let i = 0; i < n; i++) ps.push(import("./m" + i + ".mjs"));
				return Promise.all(ps).then(ms => ms.map(m => m.name).join());
			}
			function json() {
				const ps = [];
				for (let i = 0; i < 2; i++) ps.push(import("./d" + i + ".json", { with: { type: "json" } }));
				return Promise.all(ps).then(ms => ms.map(m => m.default.v).join());
			}
			function bad() {
				const ps = [];
				for (let i = 0; i < 2; i++) ps.push(import({ toString() { throw "S" + i } }).catch(e => e));
				return Promise.all(ps).then(r => r.join());
			}
			function meta() {
				let same = true;
				for (let i = 0; i < 3; i++) same = same && import.meta === import.meta && typeof import.meta === "object";
				return same;
			}
			export const all = Promise.all([load(3), json(), bad(), meta()]).then(r => r.join(" "));`)
		if err != nil {
			t.Fatalf("tree %v: %v", tree, err)
		}
		all, _ := ns.Get("all")
		v, err := all.Await(context.Background())
		if err != nil {
			t.Fatalf("tree %v: %v", tree, err)
		}
		return v.String() + " | " + strings.Join(got, ", "), vm.TreesBuilt() - before
	}
	want, _ := run(false)
	got, built := run(true)
	if got != want {
		t.Errorf("tree: %s\ninterpreter: %s", got, want)
	}
	if built < 4 {
		t.Errorf("%d functions built as trees, want load, json, bad and meta among them", built)
	}
	const answer = "./m0.mjs,./m1.mjs,./m2.mjs 7,7 S0,S1 true"
	if !strings.HasPrefix(got, answer+" |") {
		t.Errorf("got %s, want %s | ...", got, answer)
	}
}
