package v8bench

import (
	"context"
	_ "embed"
	"fmt"
	"runtime"
	"runtime/pprof"
	"strings"
)

// The ssr suite renders a page on the server with React 18, as a host
// behind a web server does for each request: React's own UMD builds,
// fetched into -dir, and the page, ssr.js, which is this repository's. It
// has two benchmarks:
//
//   - FirstRender is a fresh runtime's: React and the page loaded, and the
//     page rendered once, in a runtime of its own for each run -- what a
//     host making a runtime for each request pays, compiling included.
//   - Render is the steady state: one runtime's ssrRenders renders a run,
//     the runs one after another, as a host keeping its runtime renders.
//
//go:embed ssr.js
var ssrPage string

const (
	// ssrProducts is how many products a page lists.
	ssrProducts = 200
	// ssrWant is the length of React's markup for that page: a render
	// that makes any other fails the run.
	ssrWant = 106743
	// ssrRenders is how many renders a run of Render makes.
	ssrRenders = 10
	// ssrPrelude is what React's UMD builds take of a browser's globals:
	// a property, which a var in node's module would not make.
	ssrPrelude = "globalThis.self = globalThis;\n"
)

// ssrRun is the script of k renders of the page, each one's length
// checked.
func ssrRun(k int) string {
	return fmt.Sprintf(`(() => {
	for (let i = 0; i < %d; i++) {
		const n = __ssrRender(%d).length;
		if (n !== %d) throw new Error("ssr: the page is " + n + " characters long, not %d");
	}
	return "";
})()`, k, ssrProducts, ssrWant, ssrWant)
}

// ssrScripts are the scripts that load React and the page, by name, in
// order.
func ssrScripts(s *suite, src map[string]string) [][2]string {
	scripts := [][2]string{{"ssr-prelude.js", ssrPrelude}}
	for _, f := range s.files {
		scripts = append(scripts, [2]string{f, src[f]})
	}
	return append(scripts, [2]string{"ssr.js", ssrPage})
}

// loadSSR is a runtime with React and the page loaded.
func loadSSR(e Engine, s *suite, src map[string]string) Runtime {
	rt := newRuntime(e, src)
	for _, f := range ssrScripts(s, src) {
		if err := rt.Run(f[0], f[1]); err != nil {
			fail(fmt.Errorf("%s: %w", f[0], err))
		}
	}
	return rt
}

// closeRuntime closes rt, if its engine's runtimes are closed.
func closeRuntime(rt Runtime) {
	if c, ok := rt.(interface{ Close() }); ok {
		c.Close()
	}
}

// runSSR runs FirstRender and Render n times each, reporting each as it
// finishes, and returns the runtime Render ran in.
func runSSR(e Engine, s *suite, src map[string]string, n int, only string) Runtime {
	picked := func(name string) bool { return only == "" || strings.Contains(","+only+",", ","+name+",") }
	if picked("FirstRender") {
		var last Runtime
		measure("FirstRender", func() error {
			for i := 0; i < n; i++ {
				if last != nil {
					closeRuntime(last)
				}
				last = loadSSR(e, s, src)
				if _, err := last.EvalString(ssrRun(1)); err != nil {
					return err
				}
			}
			return nil
		})
		if r, ok := last.(Reporter); ok {
			r.Report("FirstRender")
		}
		closeRuntime(last)
	}
	var rt Runtime
	if picked("Render") {
		rt = loadSSR(e, s, src)
		measure("Render", func() error {
			for i := 0; i < n; i++ {
				if _, err := rt.EvalString(ssrRun(ssrRenders)); err != nil {
					return err
				}
			}
			return nil
		})
		if r, ok := rt.(Reporter); ok {
			r.Report("Render")
		}
	}
	return rt
}

// measure runs f as the benchmark name and reports it, as the fixed mode
// reports each benchmark.
func measure(name string, f func() error) {
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	t0, c0 := clock(), processCPU()
	var err error
	pprof.Do(context.Background(), pprof.Labels("bench", name), func(context.Context) { err = f() })
	d, c := clock()-t0, processCPU()-c0
	runtime.ReadMemStats(&m1)
	if err != nil {
		fail(fmt.Errorf("%s: %w", name, err))
	}
	report(name, d, c, &m0, &m1)
}

// ssrDriver is the fixed mode's driver for an external engine, which runs
// Render alone, a fresh runtime there being a process of its own: React's
// builds loaded into the global context (the prelude's load), where no
// module system is, and the page.
const ssrDriver = `%s
%s
%s
%s
(function () {
function pad(s, n, left) { s = String(s); while (s.length < n) s = left ? " " + s : s + " "; return s; }
var only = %q, n = %d;
if (only !== "" && ("," + only + ",").indexOf(",Render,") < 0) return;
var t0 = __v8benchNow();
for (var i = 0; i < n; i++) %s;
print(pad("Render", 13) + " " + pad((__v8benchNow() - t0).toFixed(1), 9, true) + " ms");
})();
`

// ssrExternal is an external engine's driver of the suite, prelude its
// load and print.
func ssrExternal(s *suite, prelude, only string, n int) string {
	var loads strings.Builder
	for _, f := range s.files {
		fmt.Fprintf(&loads, "load(%q);\n", f)
	}
	return fmt.Sprintf(ssrDriver, prelude, ssrPrelude, loads.String(), ssrPage, only, n, ssrRun(ssrRenders))
}
