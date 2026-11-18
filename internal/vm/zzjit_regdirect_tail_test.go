//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"
)

func TestJITRegDirectTailCallbacks(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const setup = `"use strict";
		function element(type, props, key) { return {type, props, key}; }
		function row(entry) { return element("row", {entry}, entry.id); }
		function withArgs(entry) { return element("row", {entry, count: arguments.length}, arguments[1]); }
		function failure(entry) { throw new RangeError("row " + entry.id); }
		function fail(entry) { return failure(entry); }
		var entries = [{id: 1}, {id: 2}, {id: 3}];
		function under(n, callback) {
			if (n === 0) return entries.map(callback);
			var result = under(n - 1, callback);
			return result;
		}
		function run(n) {
			var a = under(n, row), b = under(n, withArgs), out = [];
			for (var i = 0; i < a.length; i++) out.push(a[i].props.entry.id + ":" + b[i].props.count + ":" + b[i].key);
			try { under(n, fail); } catch (e) { out.push(e.name + ":" + e.message); }
			return out.join("|");
		}`
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			var rounds []string
			for range 12 {
				rounds = append(rounds, `run(2)`, `run(50)`)
			}
			r := jitAgreesForTest(t, setup, rounds...)
			jitNativeForTest(t, r, "under")
		})
	}
}
