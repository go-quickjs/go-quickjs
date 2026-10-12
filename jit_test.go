package quickjs_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/internal/hostaccess"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// The opt-in is valid in ordinary builds too, where it falls back. Pin its
// independence from code-generation policy and Node compatibility options.
func TestJITOption(t *testing.T) {
	for _, node := range []bool{false, true} {
		opts := []quickjs.Option{quickjs.WithJIT(), quickjs.WithoutCodeGeneration()}
		if node {
			opts = append(opts, quickjs.WithNodeQuirks())
		}
		r := quickjs.New(opts...)
		defer r.Close()
		for _, tc := range []struct{ source, want string }{
			{`function f(n) { let s=0; for(let i=0;i<n;i++) s+=i; return s } f(10000)`, "49995000"},
			{`let count=0; let x=f({valueOf() { count++; return 3 }}); x+":"+count`, "3:4"},
			{`typeof eval`, "undefined"},
			{`function z(n) { let a=0; for(let i=0;i<n;i++) a=-a; return a } Object.is(z(1),-0)`, "true"},
		} {
			if got := evalString(t, r, tc.source); got != tc.want {
				t.Fatalf("node=%v: %s = %s, want %s", node, tc.source, got, tc.want)
			}
		}
		if _, err := r.Eval(`Function("return 1")()`); err == nil {
			t.Fatal("JIT enabled blocked Function constructor")
		}
	}
}

// jitHotFunctions is a program with n hot functions, each run in turn, and
// its answer.
func jitHotFunctions(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "function f%d(n){let t=%d;for(let i=0;i<n;i++)t=(t*31+i+%d)|0;return t}\n", i, i, i)
	}
	b.WriteString("var fs=[")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "f%d,", i)
	}
	b.WriteString("];let t=0;for(let r=0;r<20;r++)for(const f of fs)t=(t+f(200))|0;t")
	return b.String()
}

// WithJITMemoryLimit bounds what the JIT holds, whatever the program: with
// more hot functions than fit, it holds no more than the limit, compiles
// what fits, and the answers are the interpreter's. It is apart from
// WithMemoryLimit, which it overrides the eighth of, and a script's
// ErrMemoryLimit is the same with the JIT on and off. A runtime without the
// JIT, or a build without it, takes the option and does nothing with it.
func TestJITMemoryLimitOption(t *testing.T) {
	src := jitHotFunctions(300)
	plain := quickjs.New()
	defer plain.Close()
	want := evalString(t, plain, src)
	for _, tc := range []struct {
		name  string
		opts  []quickjs.Option
		limit int64
	}{
		{"jit 256K", []quickjs.Option{quickjs.WithJIT(), quickjs.WithJITMemoryLimit(256 << 10)}, 256 << 10},
		{"jit 1M", []quickjs.Option{quickjs.WithJIT(), quickjs.WithJITMemoryLimit(1 << 20)}, 1 << 20},
		{"jit 4M over a memory limit", []quickjs.Option{quickjs.WithJIT(), quickjs.WithMemoryLimit(64 << 20), quickjs.WithJITMemoryLimit(4 << 20)}, 4 << 20},
		{"jit default", []quickjs.Option{quickjs.WithJIT(), quickjs.WithJITMemoryLimit(0)}, 8 << 20},
		{"no jit", []quickjs.Option{quickjs.WithJITMemoryLimit(256 << 10)}, 0},
	} {
		r := quickjs.New(tc.opts...)
		if got := evalString(t, r, src); got != want {
			t.Fatalf("%s: %s, want %s", tc.name, got, want)
		}
		st := hostaccess.VM(r).JITStats()
		t.Logf("%s: %+v", tc.name, st)
		r.Close()
		if st.Budget == 0 {
			// No JIT: in this build, or this runtime.
			if st.Compiled != 0 || st.Held != 0 {
				t.Fatalf("%s: the JIT ran: %+v", tc.name, st)
			}
			continue
		}
		if tc.limit == 0 {
			t.Fatalf("%s: a JIT budget without the JIT: %+v", tc.name, st)
		}
		if st.Budget != tc.limit || st.Held > st.Budget || st.Compiled == 0 {
			t.Fatalf("%s: %+v", tc.name, st)
		}
		if tc.limit == 256<<10 && st.Refused+st.Evicted == 0 {
			t.Fatalf("%s: the program fit: %+v", tc.name, st)
		}
	}
	// A script's memory limit is the same with the JIT on and off, whatever
	// the JIT's.
	for _, opts := range [][]quickjs.Option{nil, {quickjs.WithJIT(), quickjs.WithJITMemoryLimit(64 << 20)}} {
		r := quickjs.New(append(opts, quickjs.WithMemoryLimit(1<<20))...)
		_, err := r.Eval(`new ArrayBuffer(2 << 20)`)
		r.Close()
		if !errors.Is(err, quickjs.ErrMemoryLimit) {
			t.Fatalf("over a script's limit, with %d options: %v", len(opts), err)
		}
	}
}

// import() and import.meta in functions the JIT compiles give what they
// give in the interpreter: modules loaded by a loop, with the referrer each
// is resolved against, import attributes passed on, a specifier whose
// toString throws rejecting rather than throwing, and the module's
// import.meta read in a loop.
func TestJITImport(t *testing.T) {
	run := func(opts ...quickjs.Option) (string, vm.JITStats) {
		rt := quickjs.New(opts...)
		defer rt.Close()
		var got []string
		rt.SetModuleLoader(func(specifier, referrer string) (string, string, error) {
			got = append(got, specifier+" from "+referrer)
			if strings.HasSuffix(specifier, ".json") {
				return `{"v": 7}`, "/app/" + specifier, nil
			}
			return `export const name = "` + specifier + `";`, "/app/" + specifier, nil
		})
		ns, err := rt.EvalModule("/app/main.mjs", `
			// No closures: each runs natively (what the JIT does not compile
			// yet is left outside).
			function load(n, ps) {
				for (let i = 0; i < n; i++) ps.push(import("./m" + (i % 4) + ".mjs"));
				return ps;
			}
			function json(n, ps, opts) {
				for (let i = 0; i < n; i++) ps.push(import("./d" + (i % 2) + ".json", opts));
				return ps;
			}
			function bad(n, ps, spec) {
				for (let i = 0; i < n; i++) ps.push(import(spec));
				return ps;
			}
			function meta(n) {
				let same = 0;
				for (let i = 0; i < n; i++) if (import.meta === import.meta && typeof import.meta === "object") same++;
				return same;
			}
			const spec = { toString() { throw "S" } }, opts = { with: { type: "json" } };
			const rounds = [];
			for (let k = 0; k < 20; k++) rounds.push(Promise.all([
				Promise.all(load(5, [])).then(ms => ms.map(m => m.name).join()),
				Promise.all(json(2, [], opts)).then(ms => ms.map(m => m.default.v).join()),
				Promise.allSettled(bad(2, [], spec)).then(r => r.map(x => x.status + x.reason).join()),
				meta(30)]).then(r => r.join(" ")));
			export const all = Promise.all(rounds).then(r => r[0] + " / " + r[19]);`)
		if err != nil {
			t.Fatal(err)
		}
		all, _ := ns.Get("all")
		v, err := all.Await(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return v.String() + " | " + strings.Join(got, ", "), hostaccess.VM(rt).JITStats()
	}
	want, _ := run()
	got, st := run(quickjs.WithJIT())
	if got != want {
		t.Errorf("jit: %s\ninterpreter: %s", got, want)
	}
	if st.Budget != 0 && (st.Compiled < 4 || st.SSAEntries == 0) {
		// load, json, bad and meta among those compiled.
		t.Errorf("the functions did not run natively: %+v", st)
	}
}

// A for...of step the JIT makes by a call of Go runs the iterator's next,
// which may assign a binding the loop's function captured: the loop reads
// it again after each step, as the interpreter does, rather than the value
// it had before.
func TestJITUpvalueAfterIteratorStep(t *testing.T) {
	src := `(function () {
		let x = 0;
		function make(n) {
			let i = 0;
			return { [Symbol.iterator]() { return this }, next() { x++; i++; return { done: i > n, value: i } } };
		}
		function g(it) { let s = 0; for (const v of it) s = s * 3 + x + v; return s }
		const out = [];
		for (let k = 0; k < 200; k++) { x = k; out.push(g(make(5))) }
		return out[0] + "," + out[1] + "," + out[199] + "," + out.reduce((a, b) => (a + b) % 1000003, 0);
	})()`
	run := func(opts ...quickjs.Option) (string, vm.JITStats) {
		r := quickjs.New(opts...)
		defer r.Close()
		return evalString(t, r, src), hostaccess.VM(r).JITStats()
	}
	want, _ := run()
	if want != "358,479,24437,479494" {
		t.Fatalf("interpreter: %s", want)
	}
	got, st := run(quickjs.WithJIT())
	if got != want {
		t.Fatalf("jit: %s, interpreter: %s", got, want)
	}
	if st.Budget != 0 && st.SSAEntries == 0 {
		t.Fatalf("g did not run natively: %+v", st)
	}
}
