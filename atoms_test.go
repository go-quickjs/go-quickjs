package quickjs_test

import (
	"sync"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestAtomsSharedAcrossRuntimes runs, in runtimes made at once, a script
// that names properties every way: the built-ins' names, which a runtime
// made after the first shares with it, names of its own, symbols, indices
// and names that look like them. Each must give what one runtime alone does.
func TestAtomsSharedAcrossRuntimes(t *testing.T) {
	const source = `
		var o = { length: 1, prototype: 2, zzOwnName: 3, 4: "four", "04": "lead", 4294967295: "big" };
		var s = Symbol("mine"), t = Symbol.for("shared"); o[s] = "sym"; o[t] = "for"; o[Symbol.iterator] = "it";
		var keys = Reflect.ownKeys(o).map(String);
		var copy = JSON.parse(JSON.stringify({ zzOwnName: 1, toString: 2, valueOf: 3, nested: { length: 4 } }));
		[keys.join(), Object.keys(copy).join(), JSON.stringify(copy), Object.getOwnPropertyNames(Math).length,
		 o[s], o[t], o[Symbol.iterator], o.zzOwnName, o["4"], o["04"], o[4294967295],
		 Object.getOwnPropertyNames(Array.prototype).includes("flatMap"), typeof Math.sumPrecise].join("|")`
	one := quickjs.New()
	want := evalString(t, one, source)
	one.Close()

	const n = 8
	var wg sync.WaitGroup
	got := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rt := quickjs.New()
			defer rt.Close()
			v, err := rt.Eval(source)
			if err == nil {
				got[i] = v.String()
			}
			errs[i] = err
		}()
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("runtime %d: %v", i, errs[i])
		}
		if got[i] != want {
			t.Errorf("runtime %d:\n got %s\nwant %s", i, got[i], want)
		}
	}
}
