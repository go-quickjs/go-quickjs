package quickjs_test

import "testing"

// TestLazyGlobalsFreeze pins that Intl and Temporal, which are built the
// first time they are read, behave as the data properties they stand for
// once the global object is frozen: reading one used to replace the frozen
// accessor with a writable property, unfreezing the global (KI-13). The
// answers are node's.
func TestLazyGlobalsFreeze(t *testing.T) {
	checkEval(t, `"use strict";
		const out = [];
		Object.freeze(globalThis);
		out.push(typeof Intl.NumberFormat, Object.isFrozen(globalThis));
		const d = Object.getOwnPropertyDescriptor(globalThis, "Intl");
		out.push(d.configurable, "get" in d ? "accessor" : "data:" + d.writable);
		try { globalThis.Intl = 5; out.push("assigned") } catch (e) { out.push(e.constructor.name) }
		out.push(typeof Temporal.Now, Object.isFrozen(globalThis));
		out.join()`, "function,true,false,data:false,TypeError,object,true")
}

// TestLazyGlobalsBuildOnce pins that Intl is one namespace however it is
// reached -- its getter called again used to build a second one, which broke
// what the first had made (KI-51) -- that building it puts no constructor on
// the global object, and that assigning through an object inheriting from
// the global defines the name there, as for any writable global.
func TestLazyGlobalsBuildOnce(t *testing.T) {
	checkEval(t, `
		const g = Object.getOwnPropertyDescriptor(globalThis, "Intl").get;
		const first = g(), second = g();
		const o = Object.create(globalThis);
		o.Temporal = 7;
		[first === second, first === Intl, typeof NumberFormat, typeof Collator,
		 o.Temporal, Object.hasOwn(o, "Temporal"), typeof Temporal].join()`,
		"true,true,undefined,undefined,7,true,object")
}
