//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime/debug"
	"slices"
	"testing"
	"weak"
)

// Code judged not to pay on its first stretches (entrySlow) -- here
// invocations that do nothing -- is entered by Go again after
// jitNativeRetry calls the existing tiers made of it, and judged by what it
// does from then on (jitRetrySlow): a loop that runs long stays native.
// Judged once, for good, it had stayed in the tree tier.
func TestJITSlowCodeJudgedAgain(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	if r.jitCallThreshold <= 1 {
		t.Skip("the runtime compiles eagerly (QJS_JIT_STRESS)")
	}
	const setup = `
		var o = { k: 1 }
		function f(n) { let s = o.k; for (let i = 0; i < n; i++) s = (s * 3 + i) | 0; return s }`
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	cl := r.global.getOwn(r.atoms.intern("f")).value().Object().fn().closure
	if _, err := r.Run(compileForTest(t, `for (let k = 0; k < 400; k++) f(0)`)); err != nil {
		t.Fatal(err)
	}
	e := r.jit.hint(cl)
	if e == nil || e.ssa == nil || !e.entrySlow {
		t.Skipf("f was not judged slow on its empty invocations: %+v", e)
	}
	v, err := r.Run(compileForTest(t, `let t = 0; for (let k = 0; k < 3000; k++) t = (t + f(200)) | 0; t`))
	if err != nil {
		t.Fatal(err)
	}
	w := New(Config{})
	t.Cleanup(func() { w.Close(); w.ReleaseClosed() })
	if _, err := w.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	wv, _ := w.Run(compileForTest(t, `let t = 0; for (let k = 0; k < 3000; k++) t = (t + f(200)) | 0; t`))
	if v.Number() != wv.Number() {
		t.Fatalf("= %v, interpreter %v", v, wv)
	}
	if e = r.jit.hint(cl); e == nil || e.entrySlow || e.slowBackoff == 0 {
		t.Errorf("f, its loops long now, was not entered again: %+v", e)
	}
}

// A call left to Go (jitCallDone), decided from what it met at its fourth
// exit -- a bound function, which native code does not call -- is decided
// again when the code is compiled again (jitSeedCalls), from what the
// caches know then: the plain function it calls now, called natively.
func TestJITCallLeftToGoDecidedAgain(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	if r.jitCallThreshold <= 1 {
		t.Skip("the runtime compiles eagerly (QJS_JIT_STRESS)")
	}
	const setup = `
		function add(a, b) { let s = a; for (let i = 0; i < 2; i++) s = (s + b) | 0; return s }
		var fn = add.bind(null)
		var o = { k: 1 }
		function h(n, j) { let s = 0; for (let i = 0; i < n; i++) { s = fn(s, i) | 0; if (j) s = (s + o.k) | 0 } return s }`
	if _, err := r.Run(compileForTest(t, setup)); err != nil {
		t.Fatal(err)
	}
	for range 12 {
		if _, err := r.Run(compileForTest(t, `h(200, 0)`)); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("h")).value().Object().fn().closure
	e := r.jit.hint(cl)
	if e == nil || e.ssa == nil || !slices.Contains(e.callSites, jitCallDone) {
		t.Skipf("h's call of a bound function was not left to Go: %+v", e)
	}
	// The plain function from now on, and a count that is a string, a
	// speculation h is compiled again for.
	if _, err := r.Run(compileForTest(t, `fn = add; for (let k = 0; k < 12; k++) h('200', 0)`)); err != nil {
		t.Fatal(err)
	}
	e = r.jit.hint(cl)
	if e == nil || !slices.ContainsFunc(e.nativeCalls, func(x jitInline) bool { return x.cl != nil && x.cl.fn.Name == "add" }) &&
		!slices.ContainsFunc(e.inlines, func(x jitInline) bool { return x.cl != nil && x.cl.fn.Name == "add" }) {
		t.Errorf("h's call, left to Go, not decided again for add: reopts %d inline %d callSites %v nativeCalls %d inlines %d failed %v", e.reopts, e.inlineReopts, e.callSites, len(e.nativeCalls), len(e.inlines), e.failed)
	}
}

// A throw, which Go makes (ir.Host), ends the code's flow: what follows it
// that nothing reaches -- a switch's after its default throws -- is no
// fallthrough, and the function is compiled. Its lowering had been refused
// ("invalid fallthrough"): AWFY's Json's readValue never ran natively. Each
// answer is the interpreter's, in every tier and under every stress setting.
func TestJITThrowEndsTheFlow(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const src = `
		const P = {
			current: 'n',
			readValue() {
				switch (this.current) {
				case 'n': return 1
				case 't': return 2
				case '0': case '1': return this.current.charCodeAt(0)
				default: throw new Error('value ' + this.current)
				}
			},
		}
		function run(cs, n) {
			let s = 0
			for (let i = 0; i < n; i++) {
				P.current = cs[i % cs.length]
				try { s = (s + P.readValue()) | 0 } catch (e) { s = (s + e.message.length) | 0 }
			}
			return s
		}
		const log = []
		for (let r = 0; r < 20; r++) log.push(run(['n', 't', '0', '1'], 200), run(['n', 'x', '1'], 30))
		log.join()`
	jitDifferential(t, src)
	r := New(Config{JIT: true})
	t.Cleanup(func() { r.Close(); r.ReleaseClosed() })
	if _, err := r.Run(compileForTest(t, `function f(c) { switch (c) { case 1: return 'a'; default: throw new Error('no') } }`)); err != nil {
		t.Fatal(err)
	}
	cl := r.global.getOwn(r.atoms.intern("f")).value().Object().fn().closure
	for _, kind := range []jitLowerKind{jitLowerInline, jitLowerHot} {
		p, err := r.jitLower(cl.fn, kind)
		if err == nil {
			err = p.Validate()
		}
		if err != nil {
			t.Errorf("a function whose switch's default throws was refused (kind %d): %v", kind, err)
		}
	}
}

// A native callee's call of a function native callers no longer call
// (notNative) is Go's, native code going on after it (mir's call):
// no native level is unwound, where Go had run the callee from its start
// (abi.ExitEnter) and made and finished the frame of every native level
// below -- AWFY's Json, its readValue native, at each null, eight levels
// deep. Each answer is the interpreter's.
func TestJITCallsOfNotNativeCalleesAreGos(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	const setup = `
		function leaf(x) { return String(x).length + x }
		function mid(x) { let s = 0; for (let j = 0; j < 2; j++) s = (s + leaf(x + j)) | 0; return s }
		function outer(n) { let s = 0; for (let i = 0; i < n; i++) s = (s + mid(i)) | 0; return s }`
	want := New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	r := jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup+`; outer(300); outer(300)`)); err != nil {
			t.Fatal(err)
		}
	}
	leaf := r.jit.cache[weak.Make(r.global.getOwn(r.atoms.intern("leaf")).value().Object().fn().closure.fn)]
	mid := r.jit.cache[weak.Make(r.global.getOwn(r.atoms.intern("mid")).value().Object().fn().closure.fn)]
	if leaf == nil || leaf.ssa == nil || mid == nil || mid.ssa == nil || mid.link.in == 0 {
		t.Skipf("mid not called natively, or leaf without code: %+v %+v", mid, leaf)
	}
	leaf.notNative, leaf.link.entry, leaf.nativeRetry = true, 0, 0
	unwound, goCalls := r.jit.unwound, r.jit.goCalls
	wv, err := want.Run(compileForTest(t, `outer(300)`))
	if err != nil {
		t.Fatal(err)
	}
	gv, err := r.Run(compileForTest(t, `outer(300)`))
	if err != nil || gv.Number() != wv.Number() {
		t.Fatalf("outer(300) = %v, %v; interpreter %v", gv, err, wv)
	}
	if n := r.jit.unwound - unwound; n != 0 || r.jit.goCalls == goCalls {
		t.Errorf("leaf's calls unwound %d native levels, Go made %d", n, r.jit.goCalls-goCalls)
	}
}

// An inlined callee's concat (ir.StringConcat) joins its own parts: its
// Extra counts them, which the inliner does not move as it moves the
// callee's slots past the caller's (ssa's shifted). Moved, the concat read
// its parts that far past them -- the caller's slots, or past every slot,
// which panicked: test262's decodeURI tests, decimalToPercentHexString
// inlined into their loops, here as test262 has them. The runtime has no
// memory limit: under jitDifferential's, the JIT's budget refuses the
// script's top level (jitCompiles), and nothing is inlined into its loops.
func TestJITInlinedConcat(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	const src = `
		function Test262Error(m) { this.message = m }
		function decimalToHexString(n) {
		  var hex = "0123456789ABCDEF";
		  n >>>= 0;
		  var s = "";
		  while (n) {
		    s = hex[n & 0xf] + s;
		    n >>>= 4;
		  }
		  while (s.length < 4) {
		    s = "0" + s;
		  }
		  return s;
		}

		function decimalToPercentHexString(n) {
		  var hex = "0123456789ABCDEF";
		  return "%" + hex[(n >> 4) & 0xf] + hex[n & 0xf];
		}
		var errorCount = 0;
		var count = 0;
		var indexP;
		var indexO = 0;

		for (var indexB = 0xE0; indexB <= 0xEF; indexB++) {
		  count++;
		  var hexB = decimalToPercentHexString(indexB);
		  var result = true;
		  for (var indexC = 0xC0; indexC <= 0xFF; indexC++) {
		    var hexC = decimalToPercentHexString(indexC);
		    try {
		      decodeURI(hexB + "%A0" + hexC);
		    } catch (e) {
		      if ((e instanceof URIError) === true) continue;
		    }
		    result = false;
		  }
		  if (result !== true) {
		    if (indexO === 0) {
		      indexO = indexB;
		    } else {
		      if ((indexB - indexP) !== 1) {
		        if ((indexP - indexO) !== 0) {
		          var hexP = decimalToHexString(indexP);
		          var hexO = decimalToHexString(indexO);
		          throw new Test262Error('#' + hexO + '-' + hexP + ' ');
		        }
		        else {
		          var hexP = decimalToHexString(indexP);
		          throw new Test262Error('#' + hexP + ' ');
		        }
		        indexO = indexB;
		      }
		    }
		    indexP = indexB;
		    errorCount++;
		  }
		}

		if (errorCount > 0) {
		  if ((indexP - indexO) !== 0) {
		    var hexP = decimalToHexString(indexP);
		    var hexO = decimalToHexString(indexO);
		    throw new Test262Error('#' + hexO + '-' + hexP + ' ');
		  } else {
		    var hexP = decimalToHexString(indexP);
		    throw new Test262Error('#' + hexP + ' ');
		  }
		  throw new Test262Error('Total error: ' + errorCount + ' bad Unicode character in ' + count + ' ');
		}
		errorCount + ':' + count`
	for _, c := range []jitStressConfig{
		{threshold: true},
		{threshold: true, budget: 1},
		{threshold: true, budget: 3, deopt: 7},
	} {
		r := New(Config{JIT: true})
		r.jitCallThreshold, r.jitICThreshold = 1, 1
		r.jitStress = c
		v, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		if s, err := r.ToString(v); err != nil || s.Go() != "0:16" {
			t.Errorf("%+v: %v %v, want 0:16", c, s, err)
		}
		if r.JITStats().SSAEntries == 0 {
			t.Errorf("%+v: the script did not run natively", c)
		}
		r.Close()
	}
}
