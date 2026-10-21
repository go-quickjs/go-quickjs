package v8bench

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

// QuickJS's micro-benchmarks.
//
// microbench.js is QuickJS's tests/microbench.js, as it is there. Its own
// harness times each test over runs too short for some engines' clocks and
// keeps the shortest, which does not measure every engine alike: C
// QuickJS's empty loop comes out three times what a long run of it takes.
// So the micro mode gives each test the same work every time, as the fixed
// mode does the V8 suite -- the work QuickJS 2026-06-04 takes about 150 ms
// for on an AMD Ryzen 5 3600, the figures docs/benchmarks.md compares --
// runs it -n times, and prints the fastest time of one operation, in
// nanoseconds. One driver does this on every engine: microbench.js's tests,
// without the call of its main that ends the file, microbench_extra.js's,
// and then the loop.
//
// microbench_extra.js is ours: tests of what microbench.js has none of --
// generators, async functions, try statements, iterators, using
// declarations, Math.random, the in operator, a delete of an object's
// last property, BigInts beyond microbench.js's sums and products --
// written as its tests are. A test of
// async functions returns a promise of its count, which the loop awaits, so
// its time takes in the jobs it queued; every runner runs a script's jobs
// before it is done with it.

//go:embed microbench.js
var microbench string

//go:embed microbench_extra.js
var microbenchExtra string

// microWork is each test and the argument it is run with, in
// microbench.js's order. sort_bench is not among them: it reports a run's
// time over its fastest sort, not the time of an operation.
var microWork = []struct {
	name string
	n    int
}{
	{"empty_loop", 33554432}, {"empty_down_loop", 33554432}, {"empty_down_loop2", 33554432},
	{"empty_do_loop", 33554432}, {"date_now", 2097152}, {"date_parse", 32768},
	{"prop_read", 8388608}, {"prop_write", 8388608}, {"prop_update", 4194304},
	{"prop_create", 262144}, {"prop_clone", 131072}, {"prop_delete", 65536},
	{"array_read", 4194304}, {"array_write", 2097152}, {"array_update", 2097152},
	{"array_prop_create", 16384}, {"array_slice", 8192}, {"array_length_read", 8388608},
	{"array_length_decr", 4096}, {"array_hole_length_decr", 4096}, {"array_push", 8192},
	{"array_pop", 2048}, {"typed_array_read", 2097152}, {"typed_array_write", 1048576},
	{"arguments_read", 262144}, {"arguments_strict_read", 524288}, {"global_read", 8388608},
	{"global_write", 8388608}, {"global_write_strict", 8388608}, {"local_destruct", 524288},
	{"global_destruct", 524288}, {"global_destruct_strict", 524288}, {"global_func_call", 2097152},
	{"func_call", 2097152}, {"func_closure_call", 2097152}, {"int_arith", 16384},
	{"float_arith", 8192}, {"map_set_string", 512}, {"map_set_int", 1024},
	{"map_set_bigint", 1024}, {"map_delete", 1024}, {"weak_map_set", 2048},
	{"weak_map_delete", 1024}, {"array_for", 131072}, {"array_for_in", 32768},
	{"array_for_of", 32768}, {"math_min", 4096}, {"regexp_ascii", 1024},
	{"regexp_utf16", 1024}, {"regexp_replace", 256}, {"string_length", 4194304},
	{"string_build1", 2048}, {"string_build1x", 2048}, {"string_build2c", 2048},
	{"string_build2", 2048}, {"string_build3", 4096}, {"string_build4", 2048},
	{"string_build_large1", 128}, {"string_build_large2", 128}, {"int_to_string", 1048576},
	{"int_toString", 1048576}, {"float_to_string", 262144}, {"float_toString", 262144},
	{"float_toFixed", 524288}, {"float_toPrecision", 524288}, {"float_toExponential", 524288},
	{"string_to_int", 2097152}, {"string_to_float", 1048576}, {"bigint32_arith", 8192},
	{"bigint64_arith", 4096}, {"bigint256_arith", 2048},
	// microbench_extra.js's.
	{"generator_for_of", 32768}, {"generator_next", 8192}, {"generator_busy", 8192},
	{"generator_create", 524288}, {"generator_delegate", 4096}, {"async_await", 2048},
	{"async_busy", 8192}, {"async_call", 131072}, {"async_chain", 65536}, {"async_generator", 1024},
	{"try_catch", 8388608}, {"try_throw", 4194304}, {"try_finally", 8388608}, {"iterator_for_of", 8192},
	{"map_for_of", 32768}, {"using_scope", 8388608}, {"closure_make", 524288}, {"closure_counter", 4194304},
	{"closure_callback", 262144}, {"tail_self", 4194304}, {"tail_mutual", 4194304},
	{"method_call", 2097152}, {"method_call_leaf", 4194304}, {"loop_sum", 8388608},
	{"loop_arith", 2097152}, {"loop_locals", 4194304}, {"loop_elements", 8388608},
	{"loop_props", 8388608},
	{"private_field", 4194304}, {"private_method", 2097152}, {"math_random", 4194304},
	{"random_shuffle", 2097152}, {"prop_in", 1048576}, {"prop_in_keyed", 4194304},
	{"array_in", 2097152}, {"prop_delete_last", 1048576}, {"array_forEach", 32768}, {"array_reduce", 32768},
	{"array_map", 16384}, {"array_filter", 16384}, {"array_some", 32768},
	{"typed_forEach", 32768}, {"string_indexOf", 16384}, {"array_indexOf", 524288},
	{"bigint64_divmod", 131072}, {"bigint64_xorshift", 1048576}, {"bigint64_fnv1a", 16384},
	{"bigint64_search", 262144}, {"bigint64_pow", 1048576}, {"bigint1024_arith", 1024},
	{"bigint_modpow", 4096}, {"bigint_pidigits", 1024}, {"bigint_toString", 524288},
}

// microLoop is the driver's loop: each test of __work whose name begins with
// one of __only, or all of them, run __runs times, timed by microbench.js's
// own clock. A test is found by its name with eval, which sees the file's
// functions whether it runs as a script or as node's module. The loop is an
// async function's, so that it can await what a test of async functions
// returns.
const microLoop = `
var __work = %s, __only = %s, __runs = %d;
(async function () {
	console.log(pad("TEST", 24) + pad_left("N", 10) + pad_left("TIME (ns)", 12));
	for (var __i = 0; __i < __work.length; __i++) {
		var __name = __work[__i][0], __n = __work[__i][1];
		if (__only.length && !__only.some(function (p) { return __name.startsWith(p); })) continue;
		var __f = eval(__name), __best = Infinity;
		for (var __r = 0; __r < __runs; __r++) {
			var __t = get_clock(), __ops = __f(__n);
			if (__ops && typeof __ops.then === "function")
				__ops = await __ops;
			__t = get_clock() - __t;
			__best = Math.min(__best, __t * 1e6 / __ops);
		}
		console.log(pad(__name, 24) + pad_left(__n, 10) + pad_left(__best.toFixed(3), 12));
	}
})().catch(function (e) { console.log("error: " + e); });
`

// microDriver is the driver script: microbench.js without the call of its
// main that ends it, and the loop, for runs runs of the tests whose names
// begin with one of the comma-separated only, or all of them.
func microDriver(runs int, only string) (string, error) {
	end := strings.LastIndex(microbench, "\nif (typeof scriptArgs === \"undefined\")")
	if end < 0 {
		return "", fmt.Errorf("microbench.js: no call of main to leave out")
	}
	work := make([][2]any, len(microWork))
	for i, w := range microWork {
		work[i] = [2]any{w.name, w.n}
	}
	prefixes := []string{}
	if only != "" {
		prefixes = strings.Split(only, ",")
	}
	w, err := json.Marshal(work)
	if err != nil {
		return "", err
	}
	o, err := json.Marshal(prefixes)
	if err != nil {
		return "", err
	}
	return microbench[:end+1] + microbenchExtra + fmt.Sprintf(microLoop, w, o, max(runs, 1)), nil
}

// runMicro runs the driver in a runtime of e's, which is given the
// console.log and performance.now microbench.js uses where it has none, and
// returns the runtime.
func runMicro(e Engine, runs int, only string) Runtime {
	driver, err := microDriver(runs, only)
	if err != nil {
		fail(err)
	}
	rt := newRuntime(e, nil)
	const prelude = `if (typeof console === "undefined") globalThis.console = { log: print };
if (typeof performance === "undefined") globalThis.performance = { now: now };
`
	if err := rt.Run("prelude.js", prelude); err != nil {
		fail(err)
	}
	if err := rt.Run("microbench.js", driver); err != nil {
		fail(err)
	}
	return rt
}
