//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"testing"
)

// BigInts on the heap that Go makes from native code (ssa.GoArith, GoAdd)
// given back for the next result, as the tree tier gives them back
// (bigRelease): an operator's operand that is a fresh operator's result,
// and the value of a local that holds its values alone, which the
// result is stored over (ssa.CallSite's Release, the VM's soleLocals).
// The programs are the root package's TestBigIntGivenBack,
// TestBigIntSoleLocalPending and TestBigIntSoleLocals: what is given back
// and what may not be -- a value an array, a call, a string, a global, a
// closure, another local or an operand still to be taken holds -- each
// answer Node's, compiled at first call and as the JIT does by default.
var bigReleasePrograms = []struct{ src, want string }{
	{`(function () {
  var out = [];
  function g(v) { out.push(String(v)); return v; }
  function run(n) {
    var a = (1n << 200n) + 12345n, b = (1n << 300n) - 1n, c = 7n ** 90n, t, u, keep = [];
    for (var i = 0; i < n; i++) {
      t = a * b;                       // stored: never given back
      u = t + c;                       // t is a local's, not fresh
      keep.push(a * b + c, u - t, t);  // fresh sums go to an array
      keep.push((a * b) * (a * b) % c);
      keep.push(((a * b) + g(a * c)) - (b * c));
      keep.push((a * b) + (i & 1 ? b * b : c * c));
      try { keep.push((a * b) / (c - c)); } catch (e) { keep.push(e.constructor.name); }
      keep.push(((a << 100n) ^ (b >> 3n)) & (c | (a * 3n)));
      keep.push(String((a * b) + c) + ":" + ((a * b) - c));
      var p = 1n;
      for (var k = 0; k < 20; k++) p = p * (a + BigInt(k)) % b;
      keep.push(p, t, u);
      a = a + b * 3n;
      b = ((b * b) >> 300n) + 977n;
      c = c * 5n + (a % 1000n);
    }
    return keep;
  }
  var r = run(30).join(",");
  var h = 0;
  for (var i = 0; i < r.length; i++) h = (h * 31 + r.charCodeAt(i)) | 0;
  return out.length + " " + r.length + " " + h;
})()`, "30 54386 -538156609"},
	{`var h = 0n;
function f(n) {
  var s = (1n << 300n) + 12345n, m = (1n << 400n) + 7n, q = (1n << 450n) + 99n, t = 0n;
  for (var i = 0; i < n; i++) {
    s = s * (s = s * 3n % m, t = q * 3n % m, 5n) % m;
    q = q + t;
    h = (h * 31n + (s & 0xffffffffffffn)) % 1000000007n;
  }
  return n;
}
for (var k = 0; k < 20; k++) f(100);
String(h);`, "190683611"},
	{`var out = [], keep = [], saved = [], gk;
var B = 1n << 200n;
  BigInt.prototype.save = function () { "use strict"; saved.push(this); return 0n; };
  // Locals that hold their values alone: given back as they are assigned.
  function alone(n) {
    var sum = 0n, a = B + 5n, q = 1n, r = 7n, t = B;
    for (var i = 0; i < n; i++) {
      sum = sum + a * a;
      a = a + 3n;
      q = 10n * q * (a % 97n + 1n);
      r = (r * a + q) % (B + 1n);
      t = t * 3n >> 1n;
    }
    return [sum, a, q, r, t].join(",");
  }
  // A local's value copied to another local, pushed, stored to a global,
  // read a property of, crossing a jump, captured, or read by arguments:
  // each kept by something else, never given back.
  function copied(n) {
    var k = B, x;
    for (var i = 0; i < n; i++) {
      k = k * 3n + 1n;
      x = k;
      keep.push(x);
    }
    return n;
  }
  function pushed(n) {
    var k = B;
    for (var i = 0; i < n; i++) {
      k = k * 5n + 2n;
      keep.push(k);
    }
    return n;
  }
  function global(n) {
    var k = B;
    for (var i = 0; i < n; i++) {
      k = k * 7n + 1n;
      gk = k;
      keep.push(gk);
    }
    return n;
  }
  function method(n) {
    var k = B;
    for (var i = 0; i < n; i++) {
      k = k * 11n + 1n;
      k.save();
    }
    return n;
  }
  function jump(n, c) {
    var k = B, j = 1n;
    for (var i = 0; i < n; i++) {
      k = k * 13n + 1n;
      keep.push(c ? k : j);
    }
    return n;
  }
  function captured(n) {
    var k = B, get = function () { return k; };
    for (var i = 0; i < n; i++) {
      k = k * 17n + 1n;
      keep.push(get());
    }
    return n;
  }
  function args(n) {
    var k = B;
    for (var i = 0; i < n; i++) {
      k = k * 19n + 1n;
      keep.push(arguments.length === 1 ? k : 0n);
    }
    return n;
  }
  function shared(n) {
  var a0 = B * 3n, k;
  for (var i = 0; i < n; i++) {
    k = a0;
    for (var j = 0; j < 3; j++) k = k * 5n + 1n;
    keep.push(a0);
  }
  return n;
}
function returned(k) {
    for (var i = 0; i < 3; i++) k = k * 23n + 1n;
    return k;
  }
  function viaReturn(n) {
    var k = B;
    for (var i = 0; i < n; i++) {
      k = returned(k) + 0n;
      keep.push(returned(k));
    }
    return n;
  }
  out.push(alone(200), alone(200), copied(30), pushed(30), global(30), method(30), jump(30, true),
    captured(30), args(30), viaReturn(30), shared(30), keep.join(","), saved.join(","));
var s = out.join("|");
var h = 0;
for (var i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) | 0;
out.length + " " + s.length + " " + h;`, "13 23551 110140091"},
}

func TestJITBigIntGivenBack(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, stress := range []bool{true, false} {
		for i, p := range bigReleasePrograms {
			r := New(Config{JIT: true})
			if stress {
				r.jitCallThreshold, r.jitICThreshold = 1, 1
			}
			for round := range 3 {
				got, err := r.Run(compileForTest(t, p.src))
				if err != nil {
					t.Fatal(err)
				}
				if g := got.String().Go(); g != p.want {
					t.Fatalf("stress %v, program %d, round %d: %s, Node's %s", stress, i, round, g, p.want)
				}
			}
			r.Close()
			r.ReleaseClosed()
		}
	}
}

// A loop of sums and products of BigInts of 256 bits, microbench.js's
// bigint_arith, compiled: warm, its blocks are given back as it goes, as
// the tree tier gives them back, rather than allocated anew at each turn.
func TestJITBigIntGivenBackAllocs(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := New(Config{JIT: true})
	defer func() { r.Close(); r.ReleaseClosed() }()
	if _, err := r.Run(compileForTest(t, `
		function arith(n) {
			var sum = 0n, a = 1n << 123n, incr = 1n
			for (var i = 0; i < n; i++) {
				sum += a * a
				a += incr
			}
			return Number(sum % 1000003n)
		}`)); err != nil {
		t.Fatal(err)
	}
	run := compileForTest(t, `arith(1000)`)
	for range 20 {
		if _, err := r.Run(run); err != nil {
			t.Fatal(err)
		}
	}
	e := r.jit.entryFor(r.global.getOwn(r.atoms.intern("arith")).value().Object().fn().closure.fn)
	if e == nil || e.ssa == nil || e.invalid {
		t.Fatal("arith has no code")
	}
	var before, after runtime.MemStats
	made, fresh := r.jit.bigLeafs, r.jit.bigLeafNew
	runtime.ReadMemStats(&before)
	got, err := r.Run(run)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	// Node's.
	if g := got.Number(); g != 273446 {
		t.Fatalf("arith(1000) = %v, want 273446", g)
	}
	// 3000 operators made, by the leaf (jitBigLeaf), of 2000 BigInts of 4
	// and 5 words: in a few new blocks, not thousands.
	if n, m := r.jit.bigLeafs-made, r.jit.bigLeafNew-fresh; n < 3000 || m > 20 {
		t.Fatalf("arith(1000): the leaf made %d operators, in %d new blocks", n, m)
	}
	// Nor anything else allocated, where nothing does that checkptr's
	// instrumentation does not make allocate (-d=checkptr: a Value's
	// methods escape it).
	probe := heapBig(newBigWords(2, false))
	if testing.AllocsPerRun(10, func() { _ = probe.isHeapBig() }) == 0 {
		if n := after.Mallocs - before.Mallocs; n > 200 {
			t.Fatalf("arith(1000) allocated %d times", n)
		}
	}
}
