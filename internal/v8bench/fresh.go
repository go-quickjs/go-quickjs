package v8bench

import (
	"fmt"
	"strings"
)

// The fresh suite is what a host does that keeps its runtime and runs code on
// it for each request: the suites run one program for as long as they run, and
// have nothing of what code run again, or new code, finds of the code before
// it. Its bundle is the ssr suite's, React's UMD builds and the page, with a
// render; it needs the same files, in a -dir of its own or the ssr suite's.
//
//   - ProgramBundle runs the bundle as one compiled Program, again and again
//     on one runtime: a host that keeps its bundle's compiled form.
//   - EvalBundle evaluates the bundle's source, a little different each time,
//     on one runtime: a host whose code is new for each request.
//   - KeptRender renders on a runtime with the bundle loaded, each request a
//     script of its own holding the page's data as object literals: a host
//     handing a request's data to code it keeps. The literals' keys are the
//     same from one request to the next, the code that makes them is new, and
//     the code that reads them has seen the last request's.
//   - KeptProgram is KeptRender with the data's script one Program, run for
//     each request.
//   - SmallEval and SmallProgram run a small script of a few literals and a
//     function, new for each run or one Program, many times: the cost of a run
//     itself, nothing rendered.
const (
	// freshBundleRuns is how many runs of the bundle a run of ProgramBundle
	// or EvalBundle makes, and freshRequests of the page KeptRender's and
	// KeptProgram's.
	freshBundleRuns = 20
	freshRequests   = 20
	// freshProducts is how many products a request's data lists.
	freshProducts = 40
	// freshSmallRuns is how many runs of the small script SmallEval and
	// SmallProgram make.
	freshSmallRuns = 5000
	// freshSmall is the small script. Its literals read by a function it
	// makes, and by one the runtime keeps from its first run.
	freshSmall = `
var point = {x: 1, y: 2}, size = {w: 3, h: 4}, box = {x: 5, y: 6, w: 7, h: 8, tag: "box"};
function area(b) { return b.w * b.h }
var list = [];
for (var i = 0; i < 16; i++) list.push({id: i, x: i, y: i + 1, tag: "item"});
var sum = 0;
for (var j = 0; j < list.length; j++) sum += globalThis.__freshRead(list[j]);
String(area(box) + point.x + size.h + sum);
`
	// freshSmallWant is the small script's answer.
	freshSmallWant = "317"
	// freshKept is the function freshSmall's runs share.
	freshKept = `globalThis.__freshRead = function (o) { return o.x + o.y };`
)

// A ProgramRunner is a Runtime that can compile a script once and run it again
// and again: run runs it and returns its completion value, a string. A runtime
// that is not one runs the suite's Eval benchmarks alone.
type ProgramRunner interface {
	Program(name, src string) (run func() (string, error), err error)
}

// freshBundle is the bundle as one script: the suite's scripts, and a render.
func freshBundle(s *suite, src map[string]string) string {
	var b strings.Builder
	for _, f := range ssrScripts(s, src) {
		b.WriteString(f[1])
		b.WriteString(";\n")
	}
	fmt.Fprintf(&b, "String(__ssrRender(%d).length);\n", freshProducts)
	return b.String()
}

// freshData is the script of request i: the page's data as a literal, of the
// same keys and the same length for every request, rendered. It evaluates to
// the markup's length.
func freshData(i int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "String(__ssrRenderData({title: \"Shop %06d\", year: 2026, sections: [\"Home\", \"Shop\", \"About\", \"Contact\"], products: [\n", i)
	for p := range freshProducts {
		fmt.Fprintf(&b, "{id: %d, slug: \"item-%06d-%d\", name: \"Item %d of request\", description: \"The product of request %06d.\", cents: %d, stars: %d, sale: %t, tags: [\"new\", \"hot\"], reviews: [{text: \"Good\", author: \"Ann\"}, {text: \"Fine\", author: \"Bob\"}]},\n",
			p, i, p, p, i, 999+p*37, p%6, p%5 == 0)
	}
	b.WriteString("]}).length);\n")
	return b.String()
}

// freshCheck is a function that fails on an answer other than the first it is
// given, or, if want is not empty, want: a wrong render or run fails the
// benchmark rather than timing nothing.
func freshCheck(name, want string) func(string) error {
	return func(got string) error {
		if want == "" {
			if len(got) == 0 || got == "0" {
				return fmt.Errorf("%s: answered %q", name, got)
			}
			want = got
		}
		if got != want {
			return fmt.Errorf("%s: answered %q, not %q", name, got, want)
		}
		return nil
	}
}

// loadFresh is a runtime with what the kept benchmarks keep loaded.
func loadFresh(e Engine, s *suite, src map[string]string) Runtime {
	rt := loadSSR(e, s, src)
	if err := rt.Run("fresh.js", freshKept); err != nil {
		fail(fmt.Errorf("fresh.js: %w", err))
	}
	return rt
}

// runFresh runs the suite's benchmarks n times each, one runtime for each
// benchmark, kept for its runs, reporting each as it finishes. It returns the
// last runtime.
func runFresh(e Engine, s *suite, src map[string]string, n int, only string) Runtime {
	picked := func(name string) bool { return only == "" || strings.Contains(","+only+",", ","+name+",") }
	var last Runtime
	bench := func(name, want string, setup func() (Runtime, func(i int, check func(string) error) error)) {
		if !picked(name) {
			return
		}
		rt, step := setup()
		if rt == nil {
			return
		}
		check := freshCheck(name, want)
		i := 0
		measure(name, func() error {
			for range n {
				if err := step(i, check); err != nil {
					return err
				}
				i++
			}
			return nil
		})
		if r, ok := rt.(Reporter); ok {
			r.Report(name)
		}
		if last != nil {
			closeRuntime(last)
		}
		last = rt
	}
	// eval is a benchmark's run of runs scripts made by script on rt.
	eval := func(rt Runtime, runs int, script func(i int) string) func(int, func(string) error) error {
		return func(i int, check func(string) error) error {
			for k := range runs {
				got, err := rt.EvalString(script(i*runs + k))
				if err != nil {
					return err
				}
				if err := check(got); err != nil {
					return err
				}
			}
			return nil
		}
	}
	// program is a benchmark's run of runs runs of src, compiled once, on rt.
	program := func(rt Runtime, runs int, name, src string) func(int, func(string) error) error {
		p, ok := rt.(ProgramRunner)
		if !ok {
			return nil
		}
		run, err := p.Program(name, src)
		if err != nil {
			fail(fmt.Errorf("%s: %w", name, err))
		}
		return func(_ int, check func(string) error) error {
			for range runs {
				got, err := run()
				if err != nil {
					return err
				}
				if err := check(got); err != nil {
					return err
				}
			}
			return nil
		}
	}
	bundle := freshBundle(s, src)
	bench("ProgramBundle", "", func() (Runtime, func(int, func(string) error) error) {
		rt := newRuntime(e, src)
		return rt, program(rt, freshBundleRuns, "bundle.js", bundle)
	})
	bench("EvalBundle", "", func() (Runtime, func(int, func(string) error) error) {
		rt := newRuntime(e, src)
		return rt, eval(rt, freshBundleRuns, func(i int) string { return fmt.Sprintf("%s// %d\n", bundle, i) })
	})
	bench("KeptRender", "", func() (Runtime, func(int, func(string) error) error) {
		rt := loadFresh(e, s, src)
		return rt, eval(rt, freshRequests, freshData)
	})
	bench("KeptProgram", "", func() (Runtime, func(int, func(string) error) error) {
		rt := loadFresh(e, s, src)
		return rt, program(rt, freshRequests, "data.js", freshData(0))
	})
	bench("SmallEval", freshSmallWant, func() (Runtime, func(int, func(string) error) error) {
		rt := loadFresh(e, s, src)
		return rt, eval(rt, freshSmallRuns, func(i int) string { return fmt.Sprintf("// %d\n%s", i, freshSmall) })
	})
	bench("SmallProgram", freshSmallWant, func() (Runtime, func(int, func(string) error) error) {
		rt := loadFresh(e, s, src)
		return rt, program(rt, freshSmallRuns, "small.js", freshSmall)
	})
	return last
}
