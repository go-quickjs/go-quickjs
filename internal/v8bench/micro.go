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
// last property, object and array spread, object rest, computed keys,
// spread calls, calls that nothing inlines, a recursion and a mutual one,
// the callbacks built-ins call for each element, typed
// arrays of every element type and DataViews, BigInts beyond microbench.js's sums and products,
// regular expressions as programs use them -- validating, tokenizing,
// replacing, splitting, named groups, the u flag -- objects a loop carries
// from turn to turn, and the ways keeping those out of the heap may not pay,
// JSON's documents of every kind, parsed and written, functions that
// forward their arguments, f.apply(this, arguments) --
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
	{"generator_create", 524288}, {"generator_delegate", 4096}, {"generator_objects", 16384}, {"async_await", 2048},
	{"async_busy", 8192}, {"async_call", 131072}, {"async_chain", 65536}, {"async_generator", 1024},
	{"try_catch", 8388608}, {"try_throw", 4194304}, {"try_finally", 8388608}, {"iterator_for_of", 8192},
	{"map_for_of", 32768}, {"object_for_in", 262144}, {"dict_for_in", 131072}, {"using_scope", 8388608}, {"closure_make", 524288}, {"closure_counter", 4194304},
	{"closure_callback", 262144}, {"tail_self", 4194304}, {"tail_mutual", 4194304},
	{"call_fib", 4194304}, {"call_mutual", 4194304},
	{"method_call", 2097152}, {"method_call_leaf", 4194304}, {"loop_sum", 8388608},
	{"loop_arith", 2097152}, {"loop_locals", 4194304}, {"loop_elements", 8388608},
	{"loop_props", 8388608}, {"loop_int32", 2097152},
	{"string_switch_first", 4194304}, {"string_switch_last", 1048576},
	{"string_switch_miss", 1048576}, {"string_switch_mixed", 2097152},
	{"string_switch_built", 1048576}, {"string_switch_small", 4194304},
	{"string_switch_large", 262144}, {"string_switch_prefix", 1048576},
	{"string_switch_long", 1048576}, {"string_switch_typeof", 2097152},
	{"string_switch_nonstring", 524288}, {"string_switch_nonliteral", 1048576},
	{"number_switch", 2097152}, {"typeof_test", 8388608},
	{"object_literal_fixed", 524288}, {"array_push_large", 512}, {"upvalue_string_append", 1048576},
	{"for_in_break", 2097152}, {"for_in_nested", 524288}, {"array_index_append", 512},
	{"array_push_28k", 3541504}, {"array_push_1m", 4000000},
	{"array_push_varying", 8192}, {"array_fill_down", 1048576}, {"array_new_fill_down", 1048576},
	{"array_literal_empty", 1048576},
	{"append_read_each", 524288}, {"append_pair_read", 524288}, {"append_keep_each", 524288}, {"append_keep_pair", 524288}, {"upvalue_short_append", 1048576},
	{"upvalue_two_appends", 524288}, {"upvalue_sum", 4194304},
	{"template_row", 524288}, {"template_list", 1048576}, {"template_escaped", 262144}, {"template_numbers", 2097152},
	{"local_append_once", 1048576}, {"local_append_read", 1048576}, {"local_append_keep", 1048576},
	{"has_own_call_hit", 2097152}, {"has_own_call_miss", 2097152},
	{"has_own_call_for_in", 262144}, {"has_own_method_hit", 2097152},
	{"array_push_args3", 1024}, {"array_push_args3_single", 1024},
	{"array_push_args5", 1024}, {"array_push_args5_single", 1024},
	{"private_field", 4194304}, {"private_method", 2097152}, {"math_random", 4194304},
	{"random_shuffle", 2097152}, {"prop_in", 1048576}, {"prop_in_keyed", 4194304},
	{"array_in", 2097152}, {"prop_delete_last", 1048576},
	{"object_spread", 262144}, {"object_spread_plain", 262144}, {"object_spread_class", 262144},
	{"object_spread_large", 65536}, {"object_getter_spread", 262144}, {"object_rest", 262144},
	{"object_computed", 1048576}, {"array_spread", 262144}, {"spread_call", 262144},
	{"array_forEach", 32768}, {"array_reduce", 32768},
	{"array_map", 16384}, {"array_filter", 16384}, {"array_some", 32768},
	{"array_forEach_once", 32768}, {"array_forEach_function", 32768}, {"array_forEach_this", 32768},
	{"array_forEach_user", 32768}, {"array_reduce_upvalue", 32768}, {"array_every_upvalue", 32768},
	{"array_find_upvalue", 32768}, {"array_sort", 4096},
	{"typed_forEach", 32768}, {"typed_map", 32768}, {"typed_filter", 16384}, {"typed_reduce", 32768},
	{"typed_some", 32768}, {"typed_every", 32768}, {"typed_find", 32768}, {"typed_findIndex", 32768},
	{"typed_sort", 4096},
	{"typed_i8_forEach", 32768}, {"typed_i8_reduce", 32768},
	{"typed_i8_read", 2097152}, {"typed_i8_write", 1048576},
	{"typed_u8_forEach", 32768}, {"typed_u8_reduce", 32768},
	{"typed_u8_map", 32768}, {"typed_u8_filter", 16384},
	{"typed_u8_every", 32768}, {"typed_u8_find", 32768},
	{"typed_u8_sort", 4096}, {"typed_u8_read", 2097152},
	{"typed_u8_write", 1048576}, {"typed_u8c_forEach", 32768},
	{"typed_u8c_reduce", 32768}, {"typed_u8c_read", 2097152},
	{"typed_u8c_write", 1048576}, {"typed_i16_forEach", 32768},
	{"typed_i16_reduce", 32768}, {"typed_i16_read", 1048576},
	{"typed_i16_write", 1048576}, {"typed_u16_forEach", 32768},
	{"typed_u16_reduce", 32768}, {"typed_u16_read", 1048576},
	{"typed_u16_write", 1048576}, {"typed_u32_forEach", 32768},
	{"typed_u32_reduce", 32768}, {"typed_u32_read", 1048576},
	{"typed_u32_write", 1048576}, {"typed_f32_forEach", 32768},
	{"typed_f32_reduce", 32768}, {"typed_f32_read", 1048576},
	{"typed_f32_write", 1048576}, {"typed_f64_forEach", 32768},
	{"typed_f64_reduce", 32768}, {"typed_f64_map", 32768},
	{"typed_f64_filter", 16384}, {"typed_f64_every", 32768},
	{"typed_f64_find", 32768}, {"typed_f64_sort", 4096},
	{"typed_f64_read", 1048576}, {"typed_f64_write", 1048576},
	{"typed_i64_forEach", 32768}, {"typed_i64_reduce", 32768},
	{"typed_i64_map", 32768}, {"typed_i64_filter", 16384},
	{"typed_i64_every", 32768}, {"typed_i64_find", 32768},
	{"typed_i64_sort", 4096}, {"typed_i64_read", 262144},
	{"typed_i64_write", 524288}, {"typed_u64_forEach", 16384},
	{"typed_u64_reduce", 16384}, {"typed_u64_read", 262144},
	{"typed_u64_write", 524288}, {"dataview_read", 262144},
	{"dataview_write", 262144},
	{"map_forEach", 32768}, {"set_forEach", 32768},
	{"string_indexOf", 16384}, {"array_indexOf", 524288},
	{"string_at", 2097152}, {"string_code_point_at", 2097152}, {"string_upper", 524288},
	{"string_trim", 1048576}, {"string_starts_with", 2097152}, {"string_pad_start", 1048576},
	{"string_repeat", 1048576}, {"string_slice", 1048576}, {"string_concat", 1048576},
	{"string_from_code_point", 1048576},
	{"html_escape_plain", 1048576}, {"html_escape_entities", 262144}, {"html_escape_mixed", 524288},
	{"math_imul", 4194304}, {"math_clz32", 4194304}, {"math_pow", 2097152}, {"math_atan2_hypot", 1048576},
	{"string_template", 524288}, {"string_chain", 262144},
	{"bigint64_divmod", 131072}, {"bigint64_xorshift", 1048576}, {"bigint64_fnv1a", 16384},
	{"bigint64_search", 262144}, {"bigint64_pow", 1048576}, {"bigint1024_arith", 1024},
	{"bigint_modpow", 4096}, {"bigint_pidigits", 1024}, {"bigint_toString", 524288},
	{"regexp_test", 524288}, {"regexp_exec_g", 16384}, {"regexp_sticky", 32768},
	{"regexp_replace_dollar", 131072}, {"regexp_replace_fn", 16384}, {"regexp_split", 32768},
	{"regexp_match_g", 65536}, {"regexp_named", 262144}, {"regexp_unicode", 262144},
	{"regexp_fresh", 262144}, {"regexp_property", 131072}, {"regexp_search_i", 8192}, {"regexp_search_utf16", 8192},
	{"list_walk", 4194304}, {"method_moves", 2097152}, {"method_forEach", 2097152}, {"method_closure", 262144},
	{"object_create_fresh", 524288}, {"object_create_shared", 1048576}, {"proto_literal_fresh", 524288},
	{"set_proto_fresh", 524288}, {"class_factory", 131072}, {"subclass_fresh", 131072},
	{"scope_chain", 524288}, {"construct_new_target", 524288},
	{"escape_accumulate", 16384}, {"escape_accumulate_long", 16384}, {"escape_join", 2097152}, {"escape_rare", 2097152},
	{"literal_sites_read", 2097152}, {"literal_sites_branch", 2097152}, {"literal_sites_spill", 1048576},
	{"carried_short", 8192}, {"carried_long", 32}, {"carried_fresh", 64}, {"carried_fresh_long", 1},
	{"carried_exit_3000", 256}, {"carried_exit_500", 2048}, {"carried_exit_100", 8192}, {"carried_exit_20", 32768},
	{"carried_exit_7", 131072}, {"carried_exit_mixed", 2048}, {"carried_exit_loops", 256},
	{"carried_every_2", 1024}, {"carried_every_8", 1024}, {"carried_phases", 256}, {"carried_stored", 1024},
	{"json_parse_records", 2048}, {"json_parse_small", 262144}, {"json_parse_ints", 4096},
	{"json_parse_decimals", 4096}, {"json_parse_floats", 2048}, {"json_parse_strings", 4096},
	{"json_parse_escapes", 4096}, {"json_parse_unicode", 2048}, {"json_parse_long", 4096},
	{"json_parse_dict", 2048}, {"json_parse_unique", 8192}, {"json_parse_varied", 8192},
	{"json_parse_last_differs", 4096}, {"json_parse_index_keys", 8192},
	{"json_parse_deep", 65536}, {"json_parse_duplicates", 524288}, {"json_parse_reviver", 512},
	{"json_parse_fresh", 524288}, {"json_stringify_records", 1024},
	{"json_stringify_small", 262144}, {"json_stringify_ints", 4096},
	{"json_stringify_decimals", 1024}, {"json_stringify_floats", 1024},
	{"json_stringify_strings", 2048}, {"json_stringify_escapes", 2048},
	{"json_stringify_unicode", 4096}, {"json_stringify_long", 2048},
	{"json_stringify_dict", 2048}, {"json_stringify_unique", 4096},
	{"json_stringify_varied", 4096}, {"json_stringify_last_differs", 4096},
	{"json_stringify_index_keys", 4096}, {"json_stringify_deep", 8192},
	{"json_stringify_instances", 4096}, {"json_stringify_tojson", 16384},
	{"json_stringify_getters", 4096}, {"json_stringify_dates", 131072},
	{"json_stringify_indent", 1024}, {"json_stringify_replacer", 1024},
	{"json_stringify_allow", 8192}, {"json_round_trip", 131072}, {"json_parse_pretty", 2048},
	{"json_parse_exponents", 2048}, {"json_parse_add", 2048}, {"json_parse_read", 2048},
	{"json_parse_new_layout", 262144}, {"json_stringify_new_layout", 262144},
	{"json_stringify_proto_changes", 262144}, {"json_stringify_built", 262144},
	{"forward_call0", 1048576}, {"forward_call2", 524288}, {"forward_call6", 524288},
	{"forward_poly", 524288}, {"forward_loop", 131072}, {"forward_construct", 524288},
	{"forward_array", 524288},
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
