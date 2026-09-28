package quickjs_test

import "testing"

// TestMapSetKeepPlusZero pins that a Map or Set keeps a key of -0 as +0, as
// Map.prototype.set and Set.prototype.add say, while a Map's value of -0 is
// kept as it is: the key was kept as given, so keys(), forEach and entries
// handed back -0 (KI-40). The answers are node's.
func TestMapSetKeepPlusZero(t *testing.T) {
	checkEval(t, `const z = (v) => Object.is(v, -0) ? "-0" : String(v);
		[z([...new Map().set(-0, 1).keys()][0]), z([...new Map([[-0, 1]]).keys()][0]),
		 z([...new Set().add(-0)][0]), z([...new Set([-0])][0]),
		 (() => { let k; new Map([[-0, 1]]).forEach((v, key) => k = key); return z(k) })(),
		 z([...new Set([-0]).entries()][0][0]),
		 z([...new Set([1]).symmetricDifference(new Set([-0]))][1]),
		 z(new Map([[1, -0]]).get(1)), new Map([[0, "x"]]).get(-0)].join()`,
		"0,0,0,0,0,0,0,-0,x")
}

// TestIntersectionSeesChanges pins that Set.prototype.intersection walks the
// receiver as it is, and asks for each of the argument's keys as it comes, so
// that what the argument's has or keys change during the call is seen: it
// worked on a snapshot of each (KI-41). The answers are node's.
func TestIntersectionSeesChanges(t *testing.T) {
	checkEval(t, `const out = [];
		{ const s = new Set([1, 2, 3]); const other = { size: 10, has(v) { if (v === 1) { s.delete(2); s.add(4); } return true; }, keys() { return [][Symbol.iterator](); } };
		  out.push([...s.intersection(other)].join()); }
		{ const s = new Set([1, 2, 3]); const other = { size: 10, has(v) { if (v === 1) s.delete(2); return v !== 3; }, keys() { return [][Symbol.iterator](); } };
		  out.push([...s.intersection(other)].join()); }
		{ const s = new Set([2, 3]); const other = { size: 1, has() { return true }, keys() { const it = [2, 3][Symbol.iterator](); return { next() { const r = it.next(); if (r.value === 3) s.delete(2); return r; } }; } };
		  out.push([...s.intersection(other)].join()); }
		{ const s = new Set([1, 2, 3]); const other = { size: 1, has() { return true }, keys() { const it = [3, 2, 5][Symbol.iterator](); return { next() { const r = it.next(); if (r.value === 3) s.delete(2); return r; } }; } };
		  out.push([...s.intersection(other)].join()); }
		out.join(" | ")`, "1,3,4 | 1 | 2,3 | 3")
}
