//go:build amd64 || arm64

package vm

import (
	"strings"
	"testing"
)

// asmPosSrc is an asm.js module whose functions call each other typed --
// outer, middle and inner, entered natively one from the other -- down to
// an import that makes an error's stack, one that throws, and a recursion
// past the stack's limit: the positions of each frame of the module.
const asmPosSrc = `function PosModule(stdlib, foreign, heap) {
    "use asm";
    var ffi = foreign.ffi;
    var fail = foreign.fail;
    var H32 = new stdlib.Int32Array(heap);
    function inner(x) {
        x = x | 0;
        H32[0] = x;
        return ffi(x | 0) | 0;
    }
    function middle(x) {
        x = x | 0;
        return (inner((x + 1) | 0) | 0) + 1 | 0;
    }
    function outer(x) {
        x = x | 0;
        var y = 0;
        y = middle(x) | 0;
        return y | 0;
    }
    function thrower(x) {
        x = x | 0;
        fail(x | 0);
        return x | 0;
    }
    function deep(n) {
        n = n | 0;
        if ((n | 0) == 0) return 0;
        return (deep((n - 1) | 0) | 0) + 1 | 0;
    }
    return { outer: outer, thrower: thrower, deep: deep };
}
var posStacks = [];
var posModule = PosModule(globalThis, {
    ffi: function (x) { posStacks.push(new Error("at " + x).stack); return x; },
    fail: function (x) { throw new TypeError("fail " + x); }
}, new ArrayBuffer(65536));
function posRun() {
    var out = [];
    out.push(posModule.outer(5), posModule.outer(6));
    out.push(posStacks.join("\n"));
    try { posModule.thrower(7); } catch (e) { out.push(e.stack); }
    try { posModule.deep(1e7); } catch (e) { out.push(e instanceof RangeError, e.stack); }
    return out.join("\n");
}
`

// TestAsmStackPositions runs asmPosSrc with typed code, natively and by
// the Go table, and without: every frame of the module in an error's
// stack is where the ordinary code puts it -- typed code's frames are
// found in its own line table (bytecode.TypedPC) -- and the frames typed
// code entered natively, one from the other, are among them.
func TestAsmStackPositions(t *testing.T) {
	t.Parallel()
	want, ran := asmRun(t, asmPosSrc, "posRun()", true, false, "posModule", "outer")
	if ran {
		t.Fatal("the module ran typed with typed code off")
	}
	// The import's error, made two frames under outer: inner's call of it
	// on line 9, middle's of inner on line 13, outer's of middle on 18.
	for _, at := range []string{"at inner (test:9:16)", "at middle (test:13:17)", "at Object.outer (test:18:13)"} {
		if !strings.Contains(want, at) {
			t.Fatalf("the ordinary code's stacks have no %q:\n%s", at, want)
		}
	}
	for _, c := range []struct{ native, marking bool }{{true, false}, {true, true}, {false, false}} {
		nativeMarking(t, c.marking)
		got, ran := asmRun(t, asmPosSrc, "posRun()", c.native, true, "posModule", "outer")
		if !ran {
			t.Errorf("%+v: the module did not run typed", c)
		}
		if got != want {
			t.Errorf("%+v: stacks\n%s\nwant\n%s", c, got, want)
		}
	}
	nativeMarking(t, false)
}
