//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
)

const tailBuiltinSetup = `"use strict";
var root = Math.sqrt;
function glyph(c) { return String.fromCharCode(c) }
function method(n) { return Math.sqrt(n) }
function plain(n) { return root(n) }
function leaf(n) { return Number.isFinite(n) }
function deep(n, c) { if (n) return deep(n - 1, c); return glyph(c) }
function run(n) {
  var sum = 0;
  for (var i = 0; i < n; i++) sum += glyph(i & 255).charCodeAt(0) + method(i * i) + plain(i * i) + leaf(i);
  return sum;
}`

// Tail calls use the same built-in target feedback as ordinary calls,
// both live and in a background snapshot.
func TestJITTailBuiltinFeedback(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	r.SetJITWarmUp(65535, 65535)
	if _, err := r.Run(compileForTest(t, tailBuiltinSetup+`run(10)`)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"glyph", "method", "plain", "leaf"} {
		cl := jitGlobalClosureForTest(r, name)
		p, err := jitcompile.LowerRegInline(cl.reg.rc, cl.fn)
		if err != nil {
			t.Fatal(err)
		}
		fb := r.jitRegFeedbackFor(cl, p)
		snap := jitRegSnapshotOf(fb, p)
		found := false
		for pc, in := range p.Code {
			if !in.Tail || !in.Window {
				continue
			}
			found = true
			want := bytecode.RCallMWT
			if name == "plain" {
				want = bytecode.RCallWT
			}
			if op := cl.reg.rc.Code[p.Origin[pc]].Op(); op != want {
				t.Fatalf("%s: got %v, want %v", name, op, want)
			}
			if name == "glyph" {
				x, ok := fb.FromCharCode(pc)
				y, saved := snap.FromCharCode(pc)
				if !ok || !saved || x == 0 || x != y {
					t.Fatalf("%s: missing live or saved character intrinsic", name)
				}
			} else if name == "leaf" {
				x, ok := fb.Leaf(pc)
				y, saved := snap.Leaf(pc)
				if !ok || !saved || x == 0 || x != y {
					t.Fatalf("%s: missing live or saved leaf", name)
				}
			} else {
				x, ok := fb.Intrinsic(pc)
				y, saved := snap.Intrinsic(pc)
				if !ok || !saved || x != y {
					t.Fatalf("%s: missing live or saved math intrinsic", name)
				}
			}
		}
		if !found {
			t.Fatalf("%s: no tail call", name)
		}
	}
}

func TestJITTailBuiltinGuards(t *testing.T) {
	jitMarkingForTest(t)
	jitDifferential(t, tailBuiltinSetup+`
var result = [];
for (var k = 0; k < 4; k++) result.push(run(3000));
result.push(glyph(0x3042), glyph(-1), glyph(65.75));
result.push(glyph({valueOf() { return 66 }}), method({valueOf() { return 81 }}));
result.push(leaf(Infinity), leaf('3'), leaf({valueOf() { return 3 }}));
result.push(deep(10000, 67));
try { glyph(Symbol()) } catch (e) { result.push(e instanceof TypeError) }
String.fromCharCode = function (n) { return '<' + n + '>' };
Math.sqrt = function (n) { return n + 3 };
root = function (n) { return n + 7 };
Number.isFinite = function (n) { return n === 3 };
result.push(glyph(65), method(9), plain(9), leaf(3), run(30));
result.join('|')`)
}
