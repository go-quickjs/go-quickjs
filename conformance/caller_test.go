package conformance_test

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/conformance"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

func TestCallerFeature(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		features     []string
	}{
		{"legacy caller", `
			function f() { return arguments.callee.caller; }
			function g() { return f(); }
			if (g() !== g) throw new Error("wrong caller");
			if (f.caller !== null) throw new Error("inactive caller");
		`, []string{"caller"}},
		{"strict caller hidden", `
			function f() { return f.caller; }
			function g() { "use strict"; return f(); }
			if (g() !== null) throw new Error("strict caller exposed");
		`, []string{"caller"}},
		{"standards mode", `
			const caller = Object.getOwnPropertyDescriptor(Function.prototype, 'caller');
			const args = Object.getOwnPropertyDescriptor(Function.prototype, 'arguments');
			if (caller.get !== args.get || caller.set !== args.set)
				throw new Error('restricted prototype accessors differ');
			if ((function () {}).caller !== null) throw new Error('inactive caller');
		`, nil},
		{"getter realm", `
			const realm = $262.createRealm();
			const f = realm.evalScript('(function () {})');
			const getter = Object.getOwnPropertyDescriptor(f, 'caller').get;
			if (!(getter instanceof realm.global.Function)) throw new Error('getter realm');
			let error;
			try { getter.call(function () { 'use strict'; }); } catch (e) { error = e; }
			if (!(error instanceof realm.global.TypeError)) throw new Error('error realm');
		`, []string{"caller"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stats vm.JITStats
			got, reason := runOne(&conformance.Suite{}, &conformance.Test{
				Path: "caller.js", Source: tc.source,
				Meta: conformance.Metadata{Features: tc.features, Flags: map[string]bool{"raw": true}},
			}, nil, &stats)
			if got != resultPass {
				t.Fatalf("result = %v (%s), want pass", got, reason)
			}
		})
	}
}
