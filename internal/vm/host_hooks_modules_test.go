package vm_test

import (
	"context"
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/internal/hostaccess"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// fakeSource is the data of a module source a test's host makes.
type fakeSource struct{}

func (fakeSource) ModuleSourceClassName() string { return "Fake.Source" }

// TestHostModules pins a module of a host's own kind: the source a loader
// gives that the host's compiler takes is its module, which imports from the
// modules it requests -- evaluated before it, their namespaces handed to its
// evaluation -- and exports bindings in their dead zone until it runs; its
// source phase is the host's module source, whose class the abstract module
// source's tag names; and source the compiler does not take is JavaScript.
func TestHostModules(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	r := hostaccess.VM(rt)
	files := map[string]string{
		"/dep.mjs": `import { sum } from "./kind.fake";
			globalThis.order = "dep ";
			try { sum } catch (e) { globalThis.seen = e.constructor.name }
			export const base = 40;`,
		"/kind.fake": "FAKE ./dep.mjs",
		"/fail.fake": "FAKE fail",
		"/plain.mjs": `export default 1;`,
	}
	rt.SetModuleLoader(func(specifier, referrer string) (string, string, error) {
		name := "/" + strings.TrimPrefix(specifier, "./")
		return files[name], name, nil
	})
	sourceProto := r.NewPlainObject()
	evaluations := 0
	vm.HostSetModuleCompiler(r, func(name, source string) (*vm.Module, bool, error) {
		rest, ok := strings.CutPrefix(source, "FAKE ")
		if !ok {
			return nil, false, nil
		}
		if rest == "fail" {
			return nil, true, r.ThrowRangeError("%s does not compile", name)
		}
		return vm.HostDefineModule(r, vm.HostModule{
			Name:     name,
			Requests: []string{rest},
			Exports:  []string{"sum", "unset", "default"},
			Source:   vm.Obj(vm.HostObject(sourceProto, fakeSource{})),
			Evaluate: func(namespaces []*vm.Object) ([]vm.Value, error) {
				evaluations++
				base, err := r.GetProp(vm.Obj(namespaces[0]), r.Intern("base"))
				if err != nil {
					return nil, err
				}
				if _, err := rt.Eval(`globalThis.order += "kind"`); err != nil {
					return nil, err
				}
				return []vm.Value{vm.Float(base.Number() + 2), vm.Undefined, vm.Str(vm.NewString(name))}, nil
			},
		}), true, nil
	})
	ns, err := rt.EvalModule("/main.mjs", `
		import kind, { sum, unset } from "./kind.fake";
		import * as again from "./kind.fake";
		import source src from "./kind.fake";
		export const got = [kind, sum, typeof unset, Object.keys(again).join(), order, seen].join(" | ");
		export const source = src;
		export const later = Promise.all([
			import.source("./kind.fake").then(s => s === src),
			import.source("./plain.mjs").catch(e => e.constructor.name),
			import("./fail.fake").catch(e => e.constructor.name + ": " + e.message),
		]).then(a => a.join(" | "));
	`)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := ns.Get("got")
	if want := "/kind.fake | 42 | undefined | default,sum,unset | dep kind | ReferenceError"; got.String() != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	if evaluations != 1 {
		t.Errorf("evaluated %d times", evaluations)
	}
	src, _ := ns.Get("source")
	if d, ok := vm.HostData(hostaccess.Unwrap(src)).(fakeSource); !ok {
		t.Errorf("source import gave %v", d)
	}
	ams := rt.AbstractModuleSource()
	get, err := rt.Eval(`(ams) => Object.getOwnPropertyDescriptor(ams.prototype, Symbol.toStringTag).get`)
	if err != nil {
		t.Fatal(err)
	}
	fn, _ := get.Call(ams)
	for v, want := range map[*quickjs.Value]string{&src: "Fake.Source", &ams: "undefined"} {
		s, err := fn.CallWithThis(*v)
		if err != nil || s.String() != want {
			t.Errorf("tag = %v, %v; want %s", s, err, want)
		}
	}
	later, _ := ns.Get("later")
	l, err := later.Await(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := "true | SyntaxError | RangeError: /fail.fake does not compile"; l.String() != want {
		t.Errorf("later = %q, want %q", l, want)
	}
}
