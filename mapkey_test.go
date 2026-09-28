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
