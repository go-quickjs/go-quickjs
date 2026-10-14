package v8bench

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// The fixed driver run as a classic script, as qjs runs it: a suite file
// that assigns the globals i and k, as box2d.js assigns i, neither ends
// the loop loading the files after it -- zlib's and Typescript's had not
// run on QuickJS -- nor changes how often a benchmark runs.
func TestFixedDriverNamesItsOwn(t *testing.T) {
	src := map[string]string{
		"base.js": `function BenchmarkSuite(name, refs, benchmarks) {
				this.name = name; this.benchmarks = benchmarks; BenchmarkSuite.suites.push(this);
			}
			BenchmarkSuite.suites = [];
			function Benchmark(name, x, run) { this.run = run; this.Setup = function () {}; this.TearDown = function () {}; }
			var runs = {};`,
		"clobber.js": `i = 22; k = 99;
			new BenchmarkSuite('Clobber', [1], [new Benchmark('Clobber', true, function () { runs.Clobber = (runs.Clobber || 0) + 1; i = 1000; k = 1000; })]);`,
		"later.js": `new BenchmarkSuite('Later', [1], [new Benchmark('Later', true, function () { runs.Later = (runs.Later || 0) + 1; })]);`,
	}
	var files []string
	var table []string
	for _, f := range []string{"base.js", "clobber.js", "later.js"} {
		files = append(files, strconv.Quote(f))
		table = append(table, fmt.Sprintf("%q: %q", f, src[f]))
	}
	// load as std.loadScript does: the file as a script, in the global
	// scope.
	prelude := `var __src = {` + strings.Join(table, ", ") + `};
globalThis.load = function (f) { (0, eval)(__src[f]); };
globalThis.__v8benchNow = function () { return 0; };
var __out = [];
globalThis.print = function (s) { __out.push(s.trim().split(/ +/)[0]); };
`
	driver := fmt.Sprintf(fixedDriver, prelude, "["+strings.Join(files, ", ")+"]", "", 3)
	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.EvalFile("driver.js", driver); err != nil {
		t.Fatal(err)
	}
	v, err := rt.Eval(`__out.join() + ' ' + runs.Clobber + ' ' + runs.Later`)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String(); got != "Clobber,Later 3 3" {
		t.Errorf("suites run and their benchmarks' runs: %q, want %q", got, "Clobber,Later 3 3")
	}
}
