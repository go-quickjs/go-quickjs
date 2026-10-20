//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"sync/atomic"
	"testing"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// BigInts' sums, differences and products native code has the VM's leaf
// make (jitBigLeaf, abi.Encoding's BigFast) without Go's call: of BigInts
// on the heap and held in their values, of either sign, results in blocks
// given back, a difference that cancels or shrinks below 2^64, products of
// many words, and a sum at a site that met strings too -- each of those
// the leaf leaves to Go's call -- while the collector marks, so that the
// leaf's stores go through Go's write barrier too.
const bigLeafSetup = `
var M = (1n << 61n) - 1n, P = (1n << 89n) - 1n
function sums(n, a0, b, c) {
	// Sums and differences of BigInts on the heap and held in their values,
	// of either sign, carries into a word of their own, a difference that
	// cancels to zero or shrinks below 2^64.
	var a = a0 * 1n, i, s = 0n, t = a0, u = -b, z = 0n, w = 0n, h
	for (i = 0; i < n; i++) {
		s += a
		t = t - b + c
		u = u + t - a
		z += (a - a + c) & M
		w += (t - (t - 5n)) + ((a + (1n << 64n)) - a) + ((t - 5n) - t) * 3n + ((a - (1n << 66n)) - a)
		a += 1n
	}
	h = s % P
	h = h * 1000003n + t % P
	h = h * 1000003n + u % P
	h = h * 1000003n + z % P
	return h * 1000003n + w % P
}
function products(n, a0, b, c0) {
	// Products: of one on the heap by one held in its value, of two of a
	// few words, of two of many, of either sign.
	var a = a0 * 1n, c = c0 * 1n, i, p = 0n, q = 0n, r = 0n, x, big = a0 * a0 * a0, h
	for (i = 0; i < n; i++) {
		p += (a * c) % P
		q += (a * b) % P
		r += (big * big) % P
		x = c * -a
		p += x >> 3n
		a += 1n
		c -= 1n
	}
	h = p % P
	h = h * 1000003n + q % P
	return h * 1000003n + r % P
}
function carries(n, a0) {
	// A step whose carry is a word of its own, as it crosses 2^128.
	var i, s = 0n, x = a0 * 1n
	for (i = 0; i < n; i++) {
		x += 1n
		s += x
	}
	return s % P
}
function signs(n, a0, c0) {
	// Products of either sign, by one held in its value.
	var a = a0 * 1n, c = c0 * 1n, i, p = 0n
	for (i = 0; i < n; i++) {
		p += (a * c) % P
		a += 1n
		c -= 1n
	}
	return p % P
}
function kept(n, a, b) {
	// Sums an array keeps: none given back, which the leaf is not tried
	// for, finding no block.
	var i, out = []
	for (i = 0; i < n; i++) out[i & 7] = i & 1 ? a + b : a * b
	return out.join()
}
var last
function powers(n) {
	// A sum of powers native code makes, its sum a global's too: what it
	// gives back, the power, is never on the heap, which the leaf is not
	// tried for either.
	var i, s = 1n << 70n
	for (i = 0; i < n; i++) s += BigInt(i & 1023) ** 5n
	last = s
	return s % P
}
function strmix(n, a, k) {
	// A sum that met BigInts and then strings, which the leaf leaves to
	// Go's call, and gives up.
	var i, t = a * 1n, s = a
	for (i = 0; i < n; i++) {
		t = t + 1n
		s = s + (i < k ? 1n : "x")
	}
	lastS = s
	return t % P
}
var lastS
function huge(n, a) {
	// A sum of more words than a block is given back of, in a new one.
	var i, x = a * 1n
	for (i = 0; i < n; i++) x = x + 1n
	return x % P
}
function karatsuba(n, a) {
	// Products of 79 words by 79 and by 41, by Karatsuba's method.
	var i, x = a * 1n, h = 0n
	for (i = 0; i < n; i++) {
		x = x + 1n
		h = (h + (x * x) % P + (x * (x >> 2500n)) % P) % P
	}
	return h
}
function divisions(n, a0, b0, c) {
	// Quotients and remainders: of divisors of one word and of many, of
	// either sign, a dividend below its divisor, results held in their
	// values, of two words, of more; a modular power's, its remainders in
	// blocks given back; a division by zero, which Go's call throws for.
	var i, a = a0 * 1n, b = b0 * 1n, s = 0n, t = 0n, r = 1n, z = "", zero = b - b
	for (i = 0; i < n; i++) {
		try {
			s += a % (i == n - 1 ? zero : b)
		} catch (e) {
			z = e.name + ":"
		}
		s += a / b + a % b
		t += a / c - a % c
		t += (b * 3n) / a + (b * 3n) % a
		t += (a * a) / (-b) + (a * a) % (-b)
		r = r * a % b
		s += (a << 70n) / (b * b * b) + (a << 70n) % (b * b * b)
		a += 12345678901234567n
	}
	try {
		z += String(a / zero)
	} catch (e) {
		z += e.name
	}
	return (s % P) + "," + (t % P) + "," + (r % P) + "," + z
}
var xs = [0x8000000000000000800000000000000000000000000000010000000000000001fffffffffffffffen, 0x800000000000000000000000000000007fffffffffffffffn,
	0x800000000000000100000000000000000000000000000001n]
var ys = [0x800000000000000100000000000000018000000000000000n, 0x8000000000000000ffffffffffffffffn, 0x8000000000000001fffffffffffffffen]
function addbacks(n) {
	// Divisions whose guessed quotient word is one too large after its
	// correction, which algorithm D adds the divisor back for.
	var i, s = 0n
	for (i = 0; i < n; i++) s += xs[i % 3] / ys[i % 3] + xs[(i + 1) % 3] / ys[(i + 1) % 3]
	return s
}
function modz(n, a, b) {
	// A remainder by zero at the last iteration, which throws.
	var i, s = 0n
	for (i = 0; i < n; i++) s += (a + 1n) % (i == n - 1 ? 0n : b) + (a + 2n) / (i == n - 1 ? 0n : b)
	return s
}
function zeros(n) {
	var r = ""
	try {
		r = String(modz(n, (1n << 400n) + 99n, (1n << 190n) + 12345n))
	} catch (e) {
		r = e.name
	}
	return r
}
var lastR
function modpows(n) {
	// QuickJS-style bigint_modpow: r a global's too, never given back, so
	// that b = b * b % m finds no block of its room every other time but
	// the one b holds; and grows, x = x * 3n, whose block is its operand's.
	var m = (1n << 521n) - 1n, j, b, e, r, x = 1n << 200n, h = 0n
	for (j = 0; j < n; j++) {
		r = 1n
		b = (m >> 3n) + BigInt(j)
		for (e = 0xc0ffee1234567891n; e > 0n; e >>= 1n) {
			if (e & 1n) r = r * b % m
			b = b * b % m
			x = x * 3n
		}
		lastR = r
		h = (h * 31n + r % 1000003n + x % 1000003n) % 1000000007n
	}
	return h
}
function ownshare(n, a0, p, m) {
	// A local that holds another's value, then a remainder of its room,
	// none given back: that value is no block to make it in.
	var i, x, h = 0n
	for (i = 0; i < n; i++) {
		x = a0
		x = (p + BigInt(i)) * p % m
		h += x % 1000003n
	}
	return h + a0 % 1000003n
}
function generic(n, a, b) {
	// A sum through + at a site that met BigInts and strings.
	var s = a, k = "", i
	for (i = 0; i < n; i++) {
		s = s + b
		if (i == n - 1) k = k + s
	}
	return k
}
var res = 0n
function reuse(n, a0, b0) {
	// A local assigned a value another holds, then sums of it: the value
	// before each is given back only once a sum is what it holds.
	var a, b, j, i, s = 0n
	for (j = 0; j < 4; j++) {
		a = a0
		b = b0
		for (i = 0; i < n; i++) {
			a += 1n
			b = b * 3n
		}
		s += a + b
	}
	res += s
	return n
}
function squares(n, bits) {
	// QuickJS's bigint_arith: locals that hold their values alone, the
	// one before each given back as the next is stored.
	var i, sum = 0n, a = BigInt(1) << BigInt(bits), incr = BigInt(1)
	for (i = 0; i < n; i++) {
		sum += a * a
		a += incr
	}
	res += sum
	return n
}
function all(n) {
	res = 0n
	squares(n, 120)
	squares(n, 250)
	reuse(n, (1n << 130n) + 7n, 0x9e3779b97f4a7c15f39cc0605cedc834n)
	return [sums(n, (1n << 123n) + 12345n, (1n << 100n) + 777n, 3n), sums(n, -(1n << 200n), (1n << 70n) - 1n, -(1n << 66n)),
		sums(n, (1n << 128n) - 50n, (1n << 64n) + 1n, 1n), sums(n, -(1n << 128n) + 50n, -(1n << 64n) - 1n, -1n),
		sums(n, (1n << 100n) + 3n, (1n << 64n) - 3n, -(1n << 63n) - 9n), products(n, (1n << 70n) + 1n, (1n << 80n) + 3n, (1n << 64n) - 7n),
		products(n, 0xf39cc0605cedc8349e3779b97f4a7c15n, 0xfedcba9876543210ffffffffffffffffn, 0xfffffffffffffffn),
		products(n, (1n << 123n) + 99n, (1n << 127n) + 5n, 1000003n), products(n, -(1n << 190n), (1n << 255n) - 1n, -12n),
		generic(n, (1n << 100n), 7n), carries(100, (1n << 128n) - 50n), carries(100, -(1n << 192n) - 50n),
		kept(n, 1n << 100n, (1n << 90n) + 1n), powers(n), huge(n, 1n << 5000n), strmix(n, 1n << 100n, n >> 4), lastS.length, karatsuba(n >> 2, 1n << 5000n),
		divisions(n, (1n << 400n) + 99n, (1n << 190n) + 12345n, 1000003n), divisions(n, -(1n << 300n) - 5n, (1n << 64n) + 3n, -97n),
		divisions(n, 0xfedcba9876543210ffffffffffffffffeeeeeeeeeeeeeeeen, 0xffffffffffffffffffffn, (1n << 63n) + 1n), addbacks(n), zeros(n), modpows(n >> 5), ownshare(n, (1n << 520n) + 77n, (1n << 400n) + 3n, (1n << 580n) - 1n), signs(300, -(1n << 190n), -12n), signs(300, (1n << 190n), -12n), signs(300, -(1n << 130n), 12n), res % P].join(" ")
}
`

// bigLeafWant are Node's answers.
var bigLeafWant = map[string]string{
	"all(300)":  "5154026368581530051067073156748127158 -376373603908374195061625472569121348459565 618970220412380594454021369989029598295970970 -618970220412380504061223191351436648157870570 618967288030705397735655449088257778726064157 -47771589752561304087260614786520367312 -499394622430552921221885845187148992645 -618973727542188457439563586988542162336 -618973764598310551488153569290323408637 1267650600228229401496703207476 54975581388850 -1638350 1569275433846670190958947357069567204253818262612711833600,1268888540267514781771602329601,1569275433846670190958947357069567204253818262612711833600,1268888540267514781771602329601,1569275433846670190958947357069567204253818262612711833600,1268888540267514781771602329601,1569275433846670190958947357069567204253818262612711833600,1268888540267514781771602329601 1180591741005786295924 65836 2348 313 323755266650 502123248386619415587100326,-281950091702893791020463524,192540236260161026645395726,RangeError:RangeError -115155647976316334187496883,-198874158671930296467848840,10533612951858270233,RangeError:RangeError 474462956038773096108581586,-63133096012884364093337504,221289861021412276742550,RangeError:RangeError 68056473384187692696364270301095552613400 RangeError 113378225 159888670 188957950 -207944450 90709709283103150 562332574774026155706912665",
	"all(2000)": "34360177393210390792214714928857917858 -2508813914173130122791383026956904444658019 618939759182038933749536515507870233916352070 -618939759182034931090081811056903766084216070 618920194291047529749055738339381549410309857 -190257572645358111231305007428692245624 -366103671644574715080679107048658395626 -618973693988472751356179687600937875523 -618974329470586345435711053629854804836 1267650600228229401496703219376 54975581388850 -1638350 1569275433846670190958947357069567204253818262612711833600,1268888540267514781771602329601,1569275433846670190958947357069567204253818262612711833600,1268888540267514781771602329601,1569275433846670190958947357069567204253818262612711833600,1268888540267514781771602329601,1569275433846670190958947357069567204253818262612711833600,1268888540267514781771602329601 1180926830784242900800 67536 4048 1906 2172362879750 415192571540407808896005773,-272443137569921659722622321,426528495074911237738825272,RangeError:RangeError -605168899034113152854730782,-599089945143826835262299600,9684353880506018745,RangeError:RangeError 27633765852685118293236601,-262065752721391294048968606,524519282838061179372000,RangeError:RangeError 453596395105610971821286308300875567719925 RangeError 966542450 1027676110 188957950 -207944450 90709709283103150 585401838446780169077637695",
}

func TestJITBigLeaf(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			runtime.GC()
		}
	}()
	defer func() { stop.Store(true); <-done }()
	// Each function's last compile, whether a call of Go it makes is the
	// leaf's to try (ssa.CallSite's Big).
	leafed := map[string]bool{}
	jitSSAOptimized = func(fn *bytecode.Function, f *ssa.Func) {
		leafed[fn.Name] = false
		for _, b := range f.Blocks {
			for _, v := range b.Values {
				for _, c := range v.Calls() {
					leafed[fn.Name] = leafed[fn.Name] || c.Big
				}
			}
		}
	}
	defer func() { jitSSAOptimized = nil }()
	r := New(Config{JIT: true})
	want := New(Config{})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, bigLeafSetup)); err != nil {
			t.Fatal(err)
		}
	}
	var srcs []string
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
			if g, w := got.String().Go(), bigLeafWant[src]; g != w {
				t.Fatalf("JIT %v, round %d: %s = %s, want %s", rt == r, i, src, g, w)
			}
		}
	}
	// The leaf made most of them: of the 22,000 iterations of each loop,
	// some ten operators each.
	if n := r.jit.bigLeafs; n < 150000 {
		t.Errorf("the leaf made %d BigInt operators", n)
	}
	for name, want := range map[string]bool{"sums": true, "squares": true, "kept": false, "powers": false, "huge": true} {
		if got, ok := leafed[name]; !ok || got != want {
			t.Errorf("%s compiled %v, its calls the leaf's %v, want %v", name, ok, got, want)
		}
	}
	// What the leaf gave up: strmix's sum, which meets strings, and none of
	// squares' or huge's, past any block given back, made in new ones.
	for name, want := range map[string]bool{"strmix": true, "huge": false, "squares": false} {
		e := r.jit.entryFor(r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure.fn)
		if e == nil || e.ssa == nil {
			t.Fatalf("%s has no code", name)
		}
		off := false
		for _, st := range e.ssa.GoCallStates() {
			off = off || st.BigOff
		}
		if off != want {
			t.Errorf("%s: a call the leaf gave up %v, want %v", name, off, want)
		}
	}
}

// The leaf's result made in the block of the value it is stored over,
// where no block of its room was given back (jitLeafOwn): only a value the
// local holds alone (bigSoleBit), no operand of the call; for any other,
// in a new block, the value left untouched.
func TestJITBigLeafOwn(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	r := New(Config{JIT: true})
	if r.jit == nil {
		r.jit = &jitState{}
	}
	r.bigFree = new(bigFreeList)
	run := func(src string) Value {
		v, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	// a + b, of four words and three: a result of four words, in room for
	// five.
	a, b := run("(1n << 200n) + 12345n"), run("(1n << 190n) + 7n")
	st := &abi.GoCallState{Op: uint64(ssa.GoArith) | uint64(ir.Add)<<abi.GoOpIndexShift | uint64(ssa.ReleaseOld)<<abi.GoOpKindShift}
	call := func(old Value) (Value, bool) {
		ctx := &abi.Context{Host: unsafe.Pointer(r), GoResume: 1}
		ctx.GoState = uintptr(unsafe.Pointer(st))
		*(*Value)(unsafe.Pointer(&ctx.GoArgs[0])) = a
		*(*Value)(unsafe.Pointer(&ctx.GoArgs[1])) = b
		*(*Value)(unsafe.Pointer(&ctx.GoArgs[2])) = old
		made := jitBigLeaf(ctx) != 0
		v := *(*Value)(unsafe.Pointer(&ctx.Keep[0]))
		runtime.KeepAlive(st)
		return v, made
	}
	want := bigString(run("(1n << 200n) + 12345n + (1n << 190n) + 7n"), 10).Go()
	// The value before: of room for five, its digits 3 * 2^230.
	old := heapBig(newBigWords(5, false))
	copy((*BigInt)(old.ref).words(), []uint{0, 0, 0, 3 << 38, 0})
	(*BigInt)(old.ref).hdr = 4 | 5<<bigCapShift
	before := bigString(old, 10).Go()
	if v, made := call(old); !made || v.ref == old.ref || bigString(v, 10).Go() != want {
		t.Errorf("of a value the local does not hold alone: made %v, in its block %v", made, v.ref == old.ref)
	}
	if got := bigString(old, 10).Go(); got != before {
		t.Errorf("the value before is %s, was %s", got, before)
	}
	// The left operand, held alone: not its own result's block.
	(*BigInt)(a.ref).hdr |= bigSoleBit
	if v, made := call(a); !made || v.ref == a.ref || bigString(v, 10).Go() != want {
		t.Errorf("of its operand: made %v, in its block %v", made, v.ref == a.ref)
	}
	(*BigInt)(a.ref).hdr &^= bigSoleBit
	r.bigFree = new(bigFreeList)
	(*BigInt)(old.ref).hdr |= bigSoleBit
	v, made := call(old)
	switch {
	case !made:
		t.Fatalf("not made in the block of the value the local holds alone")
	case v.ref != old.ref:
		t.Errorf("made elsewhere")
	case bigString(v, 10).Go() != want:
		t.Errorf("%s, want %s", bigString(v, 10).Go(), want)
	}
	// A value held alone of too little room for the sum: not its block.
	r.bigFree = new(bigFreeList)
	small := heapBig(newBigWords(4, false))
	(*BigInt)(small.ref).hdr |= bigSoleBit
	if v, made := call(small); !made || v.ref == small.ref || bigString(v, 10).Go() != want {
		t.Errorf("of a value of too little room: made %v, in its block %v", made, v.ref == small.ref)
	}
	// A difference that shrinks to a value held in its own: never made in
	// the block of the value before, which it would give back twice.
	r.bigFree = new(bigFreeList)
	sub := &abi.GoCallState{Op: uint64(ssa.GoArith) | uint64(ir.Sub)<<abi.GoOpIndexShift | uint64(ssa.ReleaseOld)<<abi.GoOpKindShift}
	st, b = sub, run("(1n << 200n) + 12340n")
	old = heapBig(newBigWords(5, false))
	(*BigInt)(old.ref).hdr = 1 | 5<<bigCapShift | bigSoleBit
	v, made = call(old)
	if !made || bigString(v, 10).Go() != "5" {
		t.Fatalf("the difference: made %v, %s", made, bigString(v, 10).Go())
	}
	if n := r.bigFree.n[5-bigBlockFrom]; n != 2 || r.bigFree.b[5-bigBlockFrom][0] == r.bigFree.b[5-bigBlockFrom][1] {
		t.Errorf("blocks of room five given back: %d, the same twice %v", n, n == 2 && r.bigFree.b[5-bigBlockFrom][0] == r.bigFree.b[5-bigBlockFrom][1])
	}
}
