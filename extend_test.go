package quickjs_test

import (
	"errors"
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// A host extends the runtime by putting things in it: a global, or a module a
// script has to import. These cover what a host can build with.

func TestSetModule(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	if err := rt.SetModule("greet", map[string]any{
		"hello":   func(name string) string { return "hello " + name },
		"answer":  42,
		"default": func() string { return "the default" },
	}); err != nil {
		t.Fatal(err)
	}

	cases := []struct{ src, want string }{
		{`import {hello} from "greet"; export const out = hello("world")`, "hello world"},
		{`import {answer} from "greet"; export const out = answer + ""`, "42"},
		{`import d from "greet"; export const out = d()`, "the default"},
		{`import d, {hello} from "greet"; export const out = d() + "/" + hello("x")`,
			"the default/hello x"},
		{`import * as m from "greet"; export const out = Object.keys(m).join()`,
			"answer,default,hello"},
		// The import is live in the sense that matters: the same module object
		// answers every importer.
		{`import * as a from "greet"
		  import * as b from "greet"
		  export const out = (a === b) + ""`, "true"},
		// A namespace is a module namespace, whatever else it looks like.
		{`import * as m from "greet"
		  export const out = Object.prototype.toString.call(m)`, "[object Module]"},
	}
	for _, tc := range cases {
		ns, err := rt.EvalModule("main.js", tc.src)
		if err != nil {
			t.Fatalf("%s: %v", tc.src, err)
		}
		out, err := ns.Get("out")
		if err != nil {
			t.Fatal(err)
		}
		if got := out.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		// Each case is its own entry point, so the module cache must not make
		// the second one see the first.
		rt.Close()
		rt = quickjs.New()
		if err := rt.SetModule("greet", map[string]any{
			"hello":   func(name string) string { return "hello " + name },
			"answer":  42,
			"default": func() string { return "the default" },
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// A host module is reached by its own name even when a loader is installed,
// and a script importing something else still goes to the loader.
func TestSetModuleBeatsTheLoader(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	rt.SetModuleLoader(func(specifier, referrer string) (string, string, error) {
		if specifier == "fs" {
			return `export const readFile = () => "from the loader"`, "fs.js", nil
		}
		return `export const where = "loaded"`, specifier, nil
	})
	if err := rt.SetModule("fs", map[string]any{
		"readFile": func() string { return "from the host" },
	}); err != nil {
		t.Fatal(err)
	}

	ns, err := rt.EvalModule("main.js", `
		import {readFile} from "fs"
		import {where} from "./other.js"
		export const out = readFile() + "/" + where
	`)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := ns.Get("out")
	if got, want := out.String(), "from the host/loaded"; got != want {
		t.Errorf("out = %q, want %q", got, want)
	}
}

// A runtime with no loader at all can still import what the host registered,
// and still refuses everything else.
func TestSetModuleWithoutALoader(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	if err := rt.SetModule("host", map[string]any{"n": 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.EvalModule("main.js", `import {n} from "host"; if (n !== 1) throw 1`); err != nil {
		t.Fatal(err)
	}
	_, err := rt.EvalModule("other.js", `import "./elsewhere.js"`)
	if err == nil {
		t.Fatal("an import with no loader should have failed")
	}
	if !strings.Contains(err.Error(), "module loader") {
		t.Errorf("error = %v, want it to mention the missing loader", err)
	}
}

// A dynamic import reaches a host module too, since that is how a script asks
// for one it may not have.
func TestSetModuleThroughDynamicImport(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	if err := rt.SetModule("host", map[string]any{"n": 7}); err != nil {
		t.Fatal(err)
	}
	v, err := rt.Eval(`
		var out = "pending"
		import("host").then(m => { out = m.n + "" })
		out
	`)
	if err != nil {
		t.Fatal(err)
	}
	// The import settles in a job, which Eval drains before returning, so the
	// value is there by the time the next evaluation reads it.
	_ = v
	got, err := rt.Eval(`out`)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "7" {
		t.Errorf("out = %q, want 7", got.String())
	}
}

// A promise the host settles is an ordinary promise: script may await it, and
// its reactions run when the queue is drained rather than at once.
func TestHostPromise(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	var pending []*quickjs.Promise
	rt.Set("later", func() quickjs.Value {
		p := rt.NewPromise()
		pending = append(pending, p)
		return p.Value()
	})

	if _, err := rt.Eval(`
		var log = []
		var p = later()
		p.then(v => log.push("got " + v))
		log.push("after")
	`); err != nil {
		t.Fatal(err)
	}
	if v, _ := rt.Eval(`log.join()`); v.String() != "after" {
		t.Errorf("before settling, log = %q", v.String())
	}

	if err := pending[0].Resolve("it"); err != nil {
		t.Fatal(err)
	}
	if err := rt.RunJobs(); err != nil {
		t.Fatal(err)
	}
	if v, _ := rt.Eval(`log.join()`); v.String() != "after,got it" {
		t.Errorf("after settling, log = %q, want %q", v.String(), "after,got it")
	}
}

func TestHostPromiseRejection(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	var p *quickjs.Promise
	rt.Set("later", func() quickjs.Value {
		p = rt.NewPromise()
		return p.Value()
	})
	if _, err := rt.Eval(`
		var out = "pending"
		later().catch(e => { out = e.message })
	`); err != nil {
		t.Fatal(err)
	}
	p.RejectError(errors.New("it broke"))
	if err := rt.RunJobs(); err != nil {
		t.Fatal(err)
	}
	if v, _ := rt.Eval(`out`); v.String() != "it broke" {
		t.Errorf("out = %q, want %q", v.String(), "it broke")
	}
}

// An async function may await what the host settles, which is the shape every
// asynchronous host API takes.
func TestHostPromiseAwaited(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	var p *quickjs.Promise
	rt.Set("later", func() quickjs.Value {
		p = rt.NewPromise()
		return p.Value()
	})
	if _, err := rt.Eval(`
		var out = "pending"
		;(async function () { out = "got " + await later() })()
	`); err != nil {
		t.Fatal(err)
	}
	if err := p.Resolve(42); err != nil {
		t.Fatal(err)
	}
	if err := rt.RunJobs(); err != nil {
		t.Fatal(err)
	}
	if v, _ := rt.Eval(`out`); v.String() != "got 42" {
		t.Errorf("out = %q, want %q", v.String(), "got 42")
	}
}

// The pieces a host assembles a value out of.
func TestHostValueConstruction(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	o := rt.NewObject()
	if err := o.Set("n", 1); err != nil {
		t.Fatal(err)
	}
	arr, err := rt.NewArray(1, "two", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Set("items", arr); err != nil {
		t.Fatal(err)
	}
	if err := rt.Set("host", o); err != nil {
		t.Fatal(err)
	}

	v, err := rt.Eval(`[typeof host, host.n, Array.isArray(host.items), host.items.join("|")].join()`)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := v.String(), "object,1,true,1|two|true"; got != want {
		t.Errorf("= %q, want %q", got, want)
	}
}

// A host function that has to throw something other than a plain Error can say
// which, and a script sees an ordinary exception either way.
func TestHostThrow(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	rt.Set("refuse", func() error {
		return rt.Throw(rt.NewError("TypeError", "not that"))
	})
	rt.Set("fail", func() error { return errors.New("plain") })

	v, err := rt.Eval(`
		var out = []
		try { refuse() } catch (e) { out.push(e.constructor.name + ":" + e.message) }
		try { fail() } catch (e) { out.push(e.constructor.name + ":" + e.message) }
		out.join("|")
	`)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := v.String(), "TypeError:not that|Error:plain"; got != want {
		t.Errorf("= %q, want %q", got, want)
	}
}

// A Go function may hand back a promise directly, which is the shape of every
// host API that finishes later.
func TestHostFunctionReturningPromise(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	var pending *quickjs.Promise
	rt.Set("readLater", func(name string) *quickjs.Promise {
		p := rt.NewPromise()
		pending = p
		return p
	})
	if _, err := rt.Eval(`
		var out = "pending"
		;(async () => { out = await readLater("x") })()
	`); err != nil {
		t.Fatal(err)
	}
	if err := pending.Resolve("contents"); err != nil {
		t.Fatal(err)
	}
	if err := rt.RunJobs(); err != nil {
		t.Fatal(err)
	}
	if v, _ := rt.Eval(`out`); v.String() != "contents" {
		t.Errorf("out = %q, want %q", v.String(), "contents")
	}
}

// A module may export values the host built rather than Go functions.
func TestSetModuleValues(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	o := rt.NewObject()
	if err := o.Set("version", "1.2.3"); err != nil {
		t.Fatal(err)
	}
	if err := rt.SetModuleValues("host", map[string]quickjs.Value{
		"info":    o,
		"default": o,
	}); err != nil {
		t.Fatal(err)
	}
	ns, err := rt.EvalModule("main.js", `
		import info, {info as named} from "host"
		export const out = [info.version, info === named].join()
	`)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := ns.Get("out")
	if got, want := out.String(), "1.2.3,true"; got != want {
		t.Errorf("out = %q, want %q", got, want)
	}
}
