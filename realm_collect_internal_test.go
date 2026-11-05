package quickjs

import (
	"runtime"
	"testing"
	"time"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// realmsLeft collects until no more of refs go, a script of the runtime's
// run between collections, and says how many are left.
func realmsLeft(t *testing.T, rt *Runtime, refs []weak.Pointer[vm.Realm]) int {
	t.Helper()
	left := len(refs)
	for range 8 {
		runtime.GC()
		runtime.GC()
		time.Sleep(time.Millisecond)
		if _, err := rt.Eval(`0`); err != nil {
			t.Fatal(err)
		}
		left = 0
		for _, w := range refs {
			if w.Value() != nil {
				left++
			}
		}
		if left == 0 {
			break
		}
	}
	return left
}

// A host that makes a realm for each request, whichever way it runs the
// request's code, gets the realm's memory back once it has dropped it: a
// script, a compiled program, what code makes of source text, a module, a
// ShadowRealm a script makes in it, a context of a sandbox, and Go functions
// set on it, the sandbox and the runtime living on.
func TestRealmsPerRequestAreCollected(t *testing.T) {
	program, err := Compile("program.js", `var p = {a: 1}; [p].map(x => x.a);`)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		run  func(t *testing.T, re *Realm)
	}{
		{"eval", func(t *testing.T, re *Realm) {
			if _, err := re.Eval(`var a = {b: 1}; function f() { return a.b; } f();`); err != nil {
				t.Fatal(err)
			}
		}},
		{"program", func(t *testing.T, re *Realm) {
			if _, err := re.RunProgram(program); err != nil {
				t.Fatal(err)
			}
		}},
		{"code from text", func(t *testing.T, re *Realm) {
			if _, err := re.Eval(`var f = new Function("a", "return {a}"); f(1); (0, eval)("var g = 2"); eval("1");`); err != nil {
				t.Fatal(err)
			}
		}},
		{"module", func(t *testing.T, re *Realm) {
			if _, err := re.Eval(`import("request.js").then(m => { globalThis.got = m.f().v; });`); err != nil {
				t.Fatal(err)
			}
			if v, err := re.Eval(`globalThis.got`); err != nil || v.String() != "42" {
				t.Fatalf("the module's value: %v, %v", v, err)
			}
		}},
		{"shadow realm", func(t *testing.T, re *Realm) {
			if _, err := re.Eval(`var s = new ShadowRealm(); var f = s.evaluate("(function () { return {a: 1}.a; })"); f();`); err != nil {
				t.Fatal(err)
			}
		}},
		{"go function", func(t *testing.T, re *Realm) {
			if err := re.Set("host", func(n int) int { return n + 1 }); err != nil {
				t.Fatal(err)
			}
			if _, err := re.Eval(`try { host(1); host("x"); } catch (e) { e.stack; }`); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			rt := New()
			defer rt.Close()
			rt.SetModuleLoader(func(specifier, referrer string) (string, string, error) {
				return `export const answer = {v: 42}; export function f() { return answer; }`, specifier, nil
			})
			var refs []weak.Pointer[vm.Realm]
			for range 10 {
				re, err := rt.NewRealm()
				if err != nil {
					t.Fatal(err)
				}
				refs = append(refs, weak.Make(re.realm))
				c.run(t, re)
			}
			if n := realmsLeft(t, rt, refs); n != 0 {
				t.Errorf("%d of %d realms alive", n, len(refs))
			}
		})
	}
	t.Run("sandbox", func(t *testing.T) {
		rt := New()
		defer rt.Close()
		sandbox, err := rt.Eval(`({n: 0})`)
		if err != nil {
			t.Fatal(err)
		}
		var refs []weak.Pointer[vm.Realm]
		for range 10 {
			re, err := rt.NewRealm(WithSandbox(sandbox))
			if err != nil {
				t.Fatal(err)
			}
			refs = append(refs, weak.Make(re.realm))
			if _, err := re.Eval(`n = n + 1; var made = 1;`); err != nil {
				t.Fatal(err)
			}
		}
		// What a realm's script left on the sandbox -- its var's value, a
		// number -- reaches no realm.
		if n := realmsLeft(t, rt, refs); n != 0 {
			t.Errorf("%d of %d realms alive", n, len(refs))
		}
		runtime.KeepAlive(sandbox)
	})
}

// A Value of a dropped realm's keeps it, for as long as the host holds it.
func TestRealmKeptByAValue(t *testing.T) {
	rt := New()
	defer rt.Close()
	re, err := rt.NewRealm()
	if err != nil {
		t.Fatal(err)
	}
	fn, err := re.Eval(`var n = 1; (function () { return {n: ++n, a: [1, 2].map(x => x * n)}; })`)
	if err != nil {
		t.Fatal(err)
	}
	w := []weak.Pointer[vm.Realm]{weak.Make(re.realm)}
	re = nil
	if n := realmsLeft(t, rt, w); n != 1 {
		t.Fatal("the realm went with a function of it held")
	}
	v, err := fn.Call()
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Set("v", v); err != nil {
		t.Fatal(err)
	}
	got, err := rt.Eval(`JSON.stringify(v)`)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"n":2,"a":[2,4]}`; got.String() != want {
		t.Fatalf("%s, want %s", got.String(), want)
	}
	if _, err := rt.Eval(`delete globalThis.v`); err != nil {
		t.Fatal(err)
	}
	fn, v = Value{}, Value{}
	if n := realmsLeft(t, rt, w); n != 0 {
		t.Error("the realm stayed once nothing held what it made")
	}
}
