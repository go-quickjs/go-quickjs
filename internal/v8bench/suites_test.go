package v8bench

import (
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// Are We Fast Yet's modules, loaded as one script, require one another
// once each, as Node's modules do; a run checks each iteration's result,
// and a wrong one fails the run with the benchmark's name.
func TestAWFYModules(t *testing.T) {
	s := &suite{name: "awfy", files: []string{"benchmark.js", "som.js", "count.js"}, awfy: true}
	src := map[string]string{
		"benchmark.js": `class Benchmark {
			innerBenchmarkLoop(n) {
				for (let i = 0; i < n; i++) if (!this.verifyResult(this.benchmark())) return false;
				return true;
			}
		}
		exports.Benchmark = Benchmark;`,
		"som.js": `exports.loads = (exports.loads || 0) + 1;`,
		"count.js": `const { Benchmark } = require('./benchmark');
		const som = require('./som');
		require('./som');
		class Count extends Benchmark {
			constructor() { super(); this.n = 0; }
			benchmark() { return ++this.n; }
			verifyResult(r) { return r <= 3 && som.loads === 1; }
		}
		exports.newInstance = () => new Count();`,
	}
	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.EvalFile("awfy.js", awfyModules(s, src)); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Eval(awfyRun("count", 3, 1)); err != nil {
		t.Fatalf("three iterations: %v", err)
	}
	_, err := rt.Eval(awfyRun("count", 2, 2))
	if err == nil || !strings.Contains(err.Error(), "count: the result is wrong") {
		t.Fatalf("a wrong result: %v", err)
	}
}
