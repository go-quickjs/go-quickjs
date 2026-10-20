//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// BigInts of 64 bits made natively (ssa's bigint.go): Uint64s from 2^63
// to 2^64-1 as well as Int64s, the bitwise operators, shifts by a
// constant, divisions and remainders, comparisons of either, BigInt() of a
// number, BigInt.asUintN and asIntN of operators made on their operands'
// low 64 bits, loops' BigInts in unboxed phis, and boxes only exits name,
// which they make of the BigInt; each leaving where a speculation fails --
// a division by zero, the most negative int64 over -1, a shift past the
// range, BigInt() of no integer, a number partway.
const bigWordSetup = `
	var M = 1000003n
	function xorshift(n, x) {
		for (let i = 0; i < n; i++) {
			x ^= BigInt.asUintN(64, x << 13n)
			x ^= x >> 7n
			x ^= BigInt.asUintN(64, x << 17n)
		}
		return x
	}
	function fnv(n, s) {
		let h = 0xcbf29ce484222325n
		for (let i = 0; i < n; i++)
			h = BigInt.asUintN(64, (h ^ BigInt(s.charCodeAt(i % s.length))) * 0x100000001b3n)
		return h
	}
	function lcg(n, x) {
		let y = 0n, z = 0n
		for (let i = 0; i < n; i++) {
			x = BigInt.asIntN(64, x * 6364136223846793005n + 1442695040888963407n)
			y = BigInt.asIntN(32, y + (x >> 11n))
			z = BigInt.asUintN(17, z * 3n + x) + BigInt.asIntN(1, x) + BigInt.asUintN(0, x)
		}
		return [x, y, z].join()
	}
	function digits(n, x) {
		let s = 0n, c = 0
		for (let i = 0; i < n; i++) {
			let v = x + BigInt(i)
			while (v != 0n) {
				s += v % 10n
				v /= 10n
				c++
			}
		}
		return s + ":" + c
	}
	function unsigned(n, a, b) {
		let c = 0, s = 0n
		for (let i = 0; i < n; i++) {
			if (a < b) c++
			if (a >= b) c += 2
			if (a <= b) c += 4
			s = BigInt.asUintN(64, s + (a & b) + (a | b) + (a ^ b))
			a += 1n
			b -= 1n
		}
		return c + ":" + s
	}
	function shifts(n, a) {
		let s = 0n
		for (let i = 0; i < n; i++) {
			s += (a >> 3n) + (a << 2n) - (-a >> 5n)
			a += 1n
		}
		return s
	}
	function ofs(n, f) {
		let s = 0n
		for (let i = 0; i < n; i++)
			s += BigInt(i * f) + BigInt(-i) + BigInt(i % 3 - 1)
		return s
	}
	function captured(n, a) {
		// A loop's BigInt, unboxed, in a state at its loop's header, whose
		// box is made after the poll on its back edge exits to it.
		const fs = []
		for (let i = 0; i < n; i++) {
			const b = a * 2n
			if (i % 100 == 0) fs.push(() => b)
			a += 1n
		}
		let s = 0n
		for (const f of fs) s = s * 31n + f()
		return s % M
	}
	function squares(n, a) {
		// A sum of squares in the int64 range that grows past 2^63, below
		// 2^64: made as Uint64s, in a Uint64 phi that BigInt(0) starts.
		let s = BigInt(0)
		for (let i = 0; i < n; i++) {
			s += a * a
			a += 1n
		}
		return s
	}
	function all(n) {
		return [squares(n, BigInt(Math.floor(Math.sqrt(1.5 * 2 ** 63 / n)))), xorshift(n, 88172645463325252n), xorshift(n, 1n), fnv(n, "The quick brown fox"), lcg(n, -5n), lcg(n, 9223372036854775807n),
			digits(n >> 3, 9007199254740993000n), digits(n >> 3, -9223372036854775807n), unsigned(n, 0x8000000000000000n - 500n, 0x8000000000000000n + 500n),
			unsigned(n, 3n, 0xffffffffffffff00n), shifts(n, 1n << 40n), shifts(n, -(1n << 40n)), ofs(n, 3), ofs(n, -0.5 * 2), captured(n, 1n << 20n)].join(" ")
	}
	function edges(n, k, d, f) {
		let s = 0n, r = ""
		try {
			for (let i = 0; i < n; i++) {
				let x = BigInt(i) - 5n
				s += x / (i == k ? d : 7n) + x % (i == k ? d : -1n)
				s += (i == k ? -0x8000000000000000n : x) / -1n
				s += BigInt(i == k + 1 ? f : i)
				// A count one of two a join merges: not the constant before.
				s = BigInt.asIntN(64, s << (i == k + 2 ? 70n : 1n))
			}
		} catch (e) {
			r = e.name
		}
		return r + s
	}
	function change(n, k) {
		let x = 7n, s = 0n
		try {
			for (let i = 0; i < n; i++) {
				if (i == k) x = 3
				s = BigInt.asUintN(64, s * 31n + x)
				x = x * 3n % 1000000007n
			}
		} catch (e) {
			return e.name + s
		}
		return s
	}
	function all2(n) {
		return [edges(n, n - 10, 0n, 1), edges(n, n - 10, 3n, 2.5), edges(n, n - 10, 3n, NaN), edges(n, n - 10, 3n, 2 ** 63), edges(n, n - 10, 3n, -(2 ** 63)),
			edges(n, n + 10, 3n, 0), change(n, n - 3), change(n, -1)].join(" ")
	}
`

// bigWordWant are Node's answers.
var bigWordWant = map[string]string{
	"all(300)": "13835077215140154650 16336317884979378143 3581399720129331960 1350894015274530760 -1366062363211519481,-1482653928,69668 2414285108102791371,-954211466,72356 " +
		"3093:703 -3364:703 1500:18446744073709548760 1500:18446744073709370896 1370953561069626 -1370953560696774 89700 -89700 472290",
	"all2(300)": "RangeError4647998506761461734 RangeError-9150747060186627624 RangeError-9150747060186627624 11686 11686 290499906672591270 " +
		"TypeError17114752522922616119 16032958090094368353",
	"all(2000)": "13835390561849011000 14004281193724023956 16819223635877332779 15421495601258989310 -8328610087454472501,-899206120,89456 3866292002786089935,-899750956,115568 " +
		"21825:4750 -22159:4750 5504:18446744073709530400 10000:18446744073707104256 9139690414196441 -9139690397579559 3997999 -3998001 210484",
	"all2(2000)": "RangeError2323999253380730336 RangeError4647998506761464275 RangeError4647998506761464275 73406 73406 145249953336295102 " +
		"TypeError5012433408292294437 17457134204551552627",
}

func TestJITBigWords(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// The collector marks now and then, so that boxes, and the words exits
	// make of BigInts, are written through Go's write barrier too.
	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			runtime.GC()
		}
	}()
	defer func() { stop.Store(true); <-done }()
	defer func(table bool) { mir.TableExits = table }(mir.TableExits)
	// Each function's last compile, whether it called Go for a BigInt's
	// operator (ssa.GoArith), as it had where it was not made natively.
	// And whether it has a phi of Uint64s.
	goArith, u64Phi := map[string]bool{}, map[string]bool{}
	jitSSAOptimized = func(fn *bytecode.Function, f *ssa.Func) {
		goArith[fn.Name], u64Phi[fn.Name] = false, false
		for _, b := range f.Blocks {
			for _, v := range b.Values {
				if v.Op == ssa.OpPhi && v.Type == ssa.Uint64 {
					u64Phi[fn.Name] = true
				}
				for _, c := range v.Calls() {
					if c.Go == ssa.GoArith {
						goArith[fn.Name] = true
					}
				}
			}
		}
	}
	defer func() { jitSSAOptimized = nil }()
	// Exits described for Go to write the frame from, and exits whose code
	// writes it.
	for _, table := range []bool{true, false} {
		mir.TableExits = table
		r := New(Config{JIT: true})
		want := New(Config{})
		for _, rt := range []*Runtime{want, r} {
			if _, err := rt.Run(compileForTest(t, bigWordSetup)); err != nil {
				t.Fatal(err)
			}
		}
		var srcs []string
		for range 30 {
			srcs = append(srcs, "all(300)", "all2(300)")
		}
		srcs = append(srcs, "all(2000)", "all2(2000)")
		for i, src := range srcs {
			got, err := r.Run(compileForTest(t, src))
			if err != nil {
				t.Fatal(err)
			}
			if g, w := got.String().Go(), bigWordWant[src]; g != w {
				t.Fatalf("table exits %v, round %d: %s = %s, want %s", table, i, src, g, w)
			}
			interp, err := want.Run(compileForTest(t, src))
			if err != nil {
				t.Fatal(err)
			}
			if g, w := interp.String().Go(), bigWordWant[src]; g != w {
				t.Fatalf("interpreter: %s = %s, want %s", src, g, w)
			}
		}
		// Warm, each runs natively: no exit, no guard failing.
		for _, name := range []string{"squares", "xorshift", "fnv", "lcg", "digits", "unsigned", "shifts", "ofs"} {
			e := r.jit.entryFor(r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure.fn)
			if e == nil || e.ssa == nil || e.invalid {
				t.Fatalf("%s has no code", name)
			}
			hosts, guards := e.ssaStats.hosts, e.ssaStats.guards
			if _, err := r.Run(compileForTest(t, "all(300)")); err != nil {
				t.Fatal(err)
			}
			if goArith[name] {
				t.Errorf("%s calls Go for BigInt operators", name)
			}
			if name == "squares" && len(e.failed) != 0 {
				// Its sum, whose addition is GoAdd's where it is generic.
				t.Errorf("%s: generic at %v (wide %v)", name, e.failed, e.bigwide)
			}
			if name == "squares" && !u64Phi[name] {
				t.Errorf("%s: its sum is boxed on every iteration, in no phi of Uint64s", name)
			}
			if e.ssaStats.hosts != hosts || e.ssaStats.guards != guards {
				t.Errorf("%s left native code: %d host exits, %d guard failures (learned %v, wide %v, generic %v)",
					name, e.ssaStats.hosts-hosts, e.ssaStats.guards-guards, e.bigints, e.bigwide, e.failed)
			}
		}
		r.Close()
		r.ReleaseClosed()
		want.Close()
		want.ReleaseClosed()
	}
}

// A BigInt's method read natively off BigInt.prototype, as a string's off
// String.prototype (ssa.OpStringProp of a holder), its call Go's: and
// leaving where BigInt.prototype changed.
func TestJITBigIntMethods(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := New(Config{JIT: true})
	defer func() { r.Close(); r.ReleaseClosed() }()
	if _, err := r.Run(compileForTest(t, `
		var x = (1n << 255n) + 12345678901234567890n
		function str(n, x) {
			let h = 0, s
			for (let j = 0; j < n; j++) {
				s = (x + BigInt(j)).toString() + (BigInt(j) - 7n).toString(16) + (x >> BigInt(j % 200)).toString(36)
				h = (h * 31 + s.length) | 0
			}
			return h + ":" + s.length
		}
		function patched(n) {
			let r = ""
			for (let j = 0; j < n; j++) {
				if (j == n - 3) BigInt.prototype.toString = function () { return "patched" }
				r = (BigInt(j) * 3n).toString()
			}
			return r
		}`)); err != nil {
		t.Fatal(err)
	}
	// Node's.
	for i, c := range []struct{ src, want string }{
		{"str(300, x)", "-1466831389:111"}, {"str(300, x)", "-1466831389:111"}, {"str(300, x)", "-1466831389:111"},
		{"str(3000, x)", "1922571733:91"}, {"str(3000, x)", "1922571733:91"},
	} {
		got, err := r.Run(compileForTest(t, c.src))
		if err != nil {
			t.Fatal(err)
		}
		if g := got.String().Go(); g != c.want {
			t.Fatalf("round %d: %s = %s, want %s", i, c.src, g, c.want)
		}
	}
	e := r.jit.entryFor(r.global.getOwn(r.atoms.intern("str")).value().Object().fn().closure.fn)
	if e == nil || e.ssa == nil || e.invalid {
		t.Fatal("str has no code")
	}
	hosts, guards := e.ssaStats.hosts, e.ssaStats.guards
	if _, err := r.Run(compileForTest(t, "str(3000, x)")); err != nil {
		t.Fatal(err)
	}
	if e.ssaStats.hosts != hosts || e.ssaStats.guards != guards {
		t.Fatalf("str left native code: %d host exits, %d guard failures (BigInt sites %v)", e.ssaStats.hosts-hosts, e.ssaStats.guards-guards, e.bigSites)
	}
	for range 20 {
		if _, err := r.Run(compileForTest(t, "patched(50)")); err != nil {
			t.Fatal(err)
		}
	}
	got, err := r.Run(compileForTest(t, `delete BigInt.prototype.toString; BigInt.prototype.toString = function (b) { return "b" + b }; patched(500) + str(3, x)`))
	if err != nil {
		t.Fatal(err)
	}
	if g := got.String().Go(); g != "patched20853:21" {
		t.Fatalf("patched: %s, want patched20853:21", g)
	}
}

// ** compiled (ir.Pow): of numbers as Math.pow's, natively where exact and
// by Go otherwise; of BigInts in the int64 range natively (ssa.OpPowI64),
// leaving where the power is past the range and for a negative exponent;
// of BigInts on the heap by Go from native code.
const bigPowSetup = `
	function nums(n) {
		let s = 0, t = ""
		for (let i = 0; i < n; i++) {
			s += (i % 7) ** 2 + 2 ** (i % 5) + (i % 3) ** 0.5 + 1.5 ** (i % 4)
			if (i == 5) t = [1 ** NaN, (-1) ** Infinity, 0 ** -1, (-0) ** -3, 10 ** 23, 0.3 ** 3].join()
		}
		return s + ":" + t
	}
	function bigs(n, k) {
		let s = 0n, r = ""
		try {
			for (let i = 0; i < n; i++) {
				s += BigInt(i & 1023) ** 5n
				s += (-2n) ** 63n + 2n ** BigInt(i % 62) + (-3n) ** BigInt(i % 40)
				s += (i == k ? 2n : 1n) ** 63n
				s = BigInt.asIntN(64, s)
			}
		} catch (e) {
			r = e.name
		}
		return r + s
	}
	function heap(n, k) {
		let s = 0n, r = ""
		try {
			for (let i = 0; i < n; i++) {
				s += ((1n << 70n) + BigInt(i)) ** 3n % 1000003n
				s += BigInt(i) ** (i == k ? -1n : 2n)
			}
		} catch (e) {
			r = e.name
		}
		return r + s
	}
	function all(n) { return [nums(n), bigs(n, n - 10), bigs(n, -1), heap(n, n - 10), heap(n, -1)].join(" ") }
`

func TestJITPow(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// Node's.
	want := map[string]string{
		"all(200)": "4368.588095116618:NaN,NaN,Infinity,-Infinity,1.0000000000000001e+23,0.026999999999999996 7861358775649125099 -1362013261205650708 " +
			"RangeError96954741 101066878",
		"all(3000)": "66069.96356237149:NaN,NaN,Infinity,-Infinity,1.0000000000000001e+23,0.026999999999999996 3134936155625690494 -6088435881229085313 " +
			"RangeError10383401760 10475730688",
	}
	// The powers each function's compiles made: natively, of numbers or of
	// Int64s, or by Go.
	made := map[string]map[string]bool{}
	jitSSAOptimized = func(fn *bytecode.Function, f *ssa.Func) {
		if made[fn.Name] == nil {
			made[fn.Name] = map[string]bool{}
		}
		for _, b := range f.Blocks {
			for _, v := range b.Values {
				switch {
				case v.Op == ssa.OpPowF64, v.Op == ssa.OpPowI64:
					made[fn.Name][v.Op.String()] = true
				case v.Op == ssa.OpCall && len(v.Calls()) != 0 && v.Calls()[0].Go == ssa.GoArith && v.Calls()[0].Argc == int(ir.Pow):
					made[fn.Name]["go"] = true
				}
			}
		}
	}
	defer func() { jitSSAOptimized = nil }()
	r := New(Config{JIT: true})
	defer func() { r.Close(); r.ReleaseClosed() }()
	if _, err := r.Run(compileForTest(t, bigPowSetup)); err != nil {
		t.Fatal(err)
	}
	var srcs []string
	for range 20 {
		srcs = append(srcs, "all(200)")
	}
	srcs = append(srcs, "all(3000)", "all(200)")
	for i, src := range srcs {
		got, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if g := got.String().Go(); g != want[src] {
			t.Fatalf("round %d: %s = %s, want %s", i, src, g, want[src])
		}
	}
	// Compiled, which a function of ** had not been at all, and warm, where
	// nothing is past what native code makes, never leaving it.
	w := New(Config{JIT: true})
	defer func() { w.Close(); w.ReleaseClosed() }()
	if _, err := w.Run(compileForTest(t, bigPowSetup)); err != nil {
		t.Fatal(err)
	}
	warm := compileForTest(t, "nums(200) + bigs(200, -1) + heap(200, -1)")
	for range 30 {
		if _, err := w.Run(warm); err != nil {
			t.Fatal(err)
		}
	}
	for name, op := range map[string]string{"nums": "powf", "bigs": "powi64", "heap": "go"} {
		if !made[name][op] {
			t.Errorf("%s's compiles made %v, no %s", name, made[name], op)
		}
		e := w.jit.entryFor(w.global.getOwn(w.atoms.intern(name)).value().Object().fn().closure.fn)
		if e == nil || e.ssa == nil || e.invalid {
			t.Errorf("%s has no code: %+v", name, e)
			continue
		}
		hosts, guards := e.ssaStats.hosts, e.ssaStats.guards
		if _, err := w.Run(warm); err != nil {
			t.Fatal(err)
		}
		if e.ssaStats.hosts != hosts || e.ssaStats.guards != guards {
			t.Errorf("%s left native code: %d host exits, %d guard failures", name, e.ssaStats.hosts-hosts, e.ssaStats.guards-guards)
		}
	}
}

// BigInts tested natively (ssa's OpTruth), of every encoding -- zero, held
// in its value in the int64 range's encoding, its pointer word that of its
// high bits; in the int64 range, past it, on the heap -- and a loop's bit
// test, if (x & 1n), as a modular power makes it, which had left native
// code at every invocation, its guard failing again each time.
const bigTruthSetup = `
	var xs = [0n, 1n, -1n, 2n ** 63n, -(2n ** 63n), 2n ** 64n - 1n, -(2n ** 64n) + 1n, 2n ** 100n, -(2n ** 100n), 0n, -0n, 5n]
	function truths(n) {
		// BigInts tested, of every encoding: in the int64 range, past it,
		// on the heap.
		let c = 0
		for (let i = 0; i < n; i++) {
			const x = xs[i % xs.length]
			if (x) c += 1
			c += x ? 2 : 0
			if (!x) c += 4
			c += x && 8 ? 16 : 0
		}
		return c
	}
	function popcount(n, e) {
		// A bit test of a loop's BigInt, if (e & 1n), as a modular power
		// makes it.
		let c = 0
		for (let i = 0; i < n; i++)
			for (let x = e + BigInt(i); x > 0n; x >>= 1n)
				if (x & 1n) c++
		return c
	}
	function all(n) {
		return [truths(n), popcount(n, 0xc0ffee1234567891n), popcount(n, 7n)].join()
	}
`

func TestJITBigIntTruth(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := New(Config{JIT: true})
	want := New(Config{})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, bigTruthSetup)); err != nil {
			t.Fatal(err)
		}
	}
	// Node's answers.
	answers := map[string]string{"all(300)": "4575,10100,1202", "all(2000)": "30515,68781,10906"}
	srcs := []string{}
	for range 30 {
		srcs = append(srcs, "all(300)")
	}
	srcs = append(srcs, "all(2000)")
	for i, src := range srcs {
		for _, rt := range []*Runtime{r, want} {
			got, err := rt.Run(compileForTest(t, src))
			if err != nil {
				t.Fatal(err)
			}
			if g, w := got.String().Go(), answers[src]; g != w {
				t.Fatalf("JIT %v, round %d: %s = %s, want %s", rt == r, i, src, g, w)
			}
		}
	}
	for _, name := range []string{"truths", "popcount"} {
		e := r.jit.entryFor(r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure.fn)
		if e == nil || e.ssa == nil || e.invalid {
			t.Fatalf("%s has no code", name)
		}
		hosts, guards := e.ssaStats.hosts, e.ssaStats.guards
		if _, err := r.Run(compileForTest(t, "all(300)")); err != nil {
			t.Fatal(err)
		}
		if e.ssaStats.hosts != hosts || e.ssaStats.guards != guards || e.deopts != 0 {
			t.Errorf("%s left native code: %d host exits, %d guard failures, %d deoptimizations (generic %v)",
				name, e.ssaStats.hosts-hosts, e.ssaStats.guards-guards, e.deopts, e.failed)
		}
	}
}

// x.toString() of a BigInt made by a call of Go of its own (ssa.GoBigString),
// BigInt.prototype.toString checked to be the method called: of every
// encoding and sign, in radix 10 and those given, undefined among them; a
// site that meets a number too, whose call checks the function; a radix
// the method refuses, which Go's call throws for.
const bigStringSetup = `
	var x = (1n << 255n) + 12345678901234567890n, out = ""
	function strings(n, base) {
		// BigInts' strings, of every encoding and sign, in radix 10 and
		// those given -- a radix the method refuses, 1, throws.
		let h = 0
		for (let i = 0; i < n; i++) {
			const v = i % 3 == 0 ? x + BigInt(i) : i % 3 == 1 ? -BigInt(i) * 7n : (1n << 64n) - BigInt(i)
			const s = v.toString()
			const t = v.toString(base)
			const u = v.toString(undefined)
			h = (h * 31 + s.length * 7 + t.charCodeAt(t.length - 1) + u.length) | 0
			if (i == n - 1) out = s + "," + t + "," + u
		}
		return h + ":" + out
	}
	function mixed(n) {
		// A site that meets a number too: its call checks the function.
		let h = 0
		for (let i = 0; i < n; i++) {
			const v = i == n - 2 ? 255 : BigInt(i) * 1000003n
			h = (h + v.toString(16).length) | 0
		}
		return h
	}
	function bad(n) {
		let r = ""
		try {
			for (let i = 0; i < n; i++) r = BigInt(i).toString(i == n - 1 ? 1 : 10)
		} catch (e) {
			return e.name + r
		}
		return r
	}
	function all(n) {
		return [strings(n, 16), strings(n, 2), strings(n, 36), mixed(n), bad(n)].join(" ")
	}
`

func TestJITBigIntToString(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	// Each function's last compile, whether it makes a toString by its own
	// call of Go.
	own := map[string]bool{}
	jitSSAOptimized = func(fn *bytecode.Function, f *ssa.Func) {
		own[fn.Name] = false
		for _, b := range f.Blocks {
			for _, v := range b.Values {
				for _, c := range v.Calls() {
					own[fn.Name] = own[fn.Name] || c.Go == ssa.GoBigString
				}
			}
		}
	}
	defer func() { jitSSAOptimized = nil }()
	r := New(Config{JIT: true})
	want := New(Config{})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, bigStringSetup)); err != nil {
			t.Fatal(err)
		}
	}
	// Node's answers.
	answers := map[string]string{
		"all(300)":  `-410897354:18446744073709551317,fffffffffffffed5,18446744073709551317 81196230:18446744073709551317,1111111111111111111111111111111111111111111111111111111011010101,18446744073709551317 1486612935:18446744073709551317,3w5e11264sgk5,18446744073709551317 2102 RangeError298`,
		"all(2000)": `1159715328:-13993,-36a9,-13993 76777632:-13993,-11011010101001,-13993 1639384927:-13993,-asp,-13993 15702 RangeError1998`,
	}
	srcs := []string{}
	for range 30 {
		srcs = append(srcs, "all(300)")
	}
	srcs = append(srcs, "all(2000)")
	for i, src := range srcs {
		for _, rt := range []*Runtime{r, want} {
			got, err := rt.Run(compileForTest(t, src))
			if err != nil {
				t.Fatal(err)
			}
			if g, w := got.String().Go(), answers[src]; g != w {
				t.Fatalf("JIT %v, round %d: %s = %s, want %s", rt == r, i, src, g, w)
			}
		}
	}
	if !own["strings"] {
		t.Errorf("strings' toString is Go's general call")
	}
	e := r.jit.entryFor(r.global.getOwn(r.atoms.intern("strings")).value().Object().fn().closure.fn)
	if e == nil || e.ssa == nil || e.invalid {
		t.Fatal("strings has no code")
	}
	hosts, guards := e.ssaStats.hosts, e.ssaStats.guards
	if _, err := r.Run(compileForTest(t, "strings(300, 16)")); err != nil {
		t.Fatal(err)
	}
	if e.ssaStats.hosts != hosts || e.ssaStats.guards != guards {
		t.Errorf("strings left native code: %d host exits, %d guard failures", e.ssaStats.hosts-hosts, e.ssaStats.guards-guards)
	}
	// BigInt.prototype.toString replaced: the call checks its function.
	src := `(BigInt.prototype.toString = function (r) { return "x" + (r | 0) }, strings(50, 16))`
	for _, rt := range []*Runtime{r, want} {
		got, err := rt.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if g, w := got.String().Go(), "1212455616:x0,x16,x0"; g != w {
			t.Errorf("JIT %v: %s = %s, want %s", rt == r, src, g, w)
		}
	}
}
