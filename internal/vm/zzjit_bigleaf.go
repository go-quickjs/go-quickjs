//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"math"
	"math/bits"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bignum"
	"github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// A BigInt's sum, difference, product, quotient or remainder that native
// code has Go make (ssa.CallSite's Big) is made first by jitBigLeaf, in
// fewer steps than Go's call of it: the leaf decodes the call itself, makes
// a sum, a difference or a product of a few words itself, its result in a
// block given back (bigFreeList), the value before's (jitLeafOwn), or a new
// one, and the rest as bigOp does, then gives back what of its operands the
// call says, as bigRelease does -- what the tree tier's operator does,
// without the calls a call of Go from native code makes before it gets
// there (callGo, goCall, ExpandGoCall, jit.HostCall, jitHostCall,
// jitGoBig), which had cost more than the operator: as V8's code
// allocates a BigInt inline and adds its digits in a builtin of its own.
// What would throw or run anything it leaves to that call. Native code
// jumps to jitBigFast (zzjit_bigleaf_*.s), as it would to jit's callGo,
// which jitBigFast goes on to when the leaf does not make the operator.
//
// jitBigFast is, as callGo is, to the Go runtime a function the Go
// function that entered the code called -- native code touches neither
// the stack nor the link register -- so that the leaf is ordinary Go, as
// goCall is, which may grow the stack, be preempted and allocate; native
// code has saved what it keeps, as for any call of Go.

// jitBigFast is where native code jumps for such a call (abi.Encoding's
// BigFast), its context in the register jit's callGo takes it in: the
// leaf called, it goes on where Go's call would have, or to callGo.
func jitBigFast()

// jitBigFastAddr is jitBigFast's address.
func jitBigFastAddr() uintptr

// jitCallGoAddr is jit's callGo's address, where jitBigFast goes on.
var jitCallGoAddr = uintptr(jit.CallGo())

// jitBigLeafMul bounds the products the leaf makes schoolbook itself, of
// operands of at most this many words, or by one of two words at most:
// what bignum's rows, in assembly, cost more for, called as Go calls them
// -- a product of two of two words took 30 ns so. Past them it multiplies
// as bigOp does, by bignum's Mul or Sqr.
const jitBigLeafMul = 4

// jitLeafKaratsuba is the words of a product's shorter operand from which
// the leaf multiplies by bignum's Mul, Karatsuba's method: bignum's own
// threshold.
const jitLeafKaratsuba = 40

// jitBigLeaf makes the call of Go ctx's code is making (abi.Context's
// GoState) where it is a BigInt's operator it makes, returning where the
// code goes on (GoResume); 0 where it leaves it to Go's call, ctx as it
// found it. It keeps the call's account (abi.GoCallState's BigScore): a
// call it leaves costs some 20 ns more than Go's alone, one it makes some
// 30 ns less, and a call it leaves two of three times of it gives up
// (BigOff) -- one at a site that meets strings, or a quotient's by zero.
func jitBigLeaf(ctx *abi.Context) uintptr {
	if ctx.GoState == 0 {
		return 0
	}
	st := (*abi.GoCallState)(*(*unsafe.Pointer)(unsafe.Pointer(&ctx.GoState)))
	if resume := jitBigLeafMake(ctx, st); resume != 0 {
		st.BigScore = max(st.BigScore-jitLeafMade, 0)
		return resume
	}
	if st.BigScore++; st.BigScore >= jitLeafGiveUp {
		st.BigOff = true
	}
	return 0
}

// jitLeafMade is what a call the leaf makes takes off its account, each
// it leaves adding one, and jitLeafGiveUp the account at which it gives
// the call up.
const (
	jitLeafMade   = 2
	jitLeafGiveUp = 64
)

// jitLeafOwn is the block of the value the call's result is stored over
// (abi.Context's GoArgs[2]), where the call gives it back (ssa.ReleaseOld),
// nothing else has seen it (bigSoleBit) and it is no operand of the call:
// where no block of the result's room was given back, the result is made
// in it, the leaf sure by then to make the call, as if it had been given
// back first -- what bigRelease gives back only once the result is made.
// A modular power's b = b * b % m, whose blocks are taken by r = r * b % m
// as well, r a global's too, had found none every other time.
func jitLeafOwn(ctx *abi.Context, op uint64, a, b Value) *BigInt {
	if uint8(op>>abi.GoOpKindShift)&ssa.ReleaseOld == 0 {
		return nil
	}
	old := *(*Value)(unsafe.Pointer(&ctx.GoArgs[2]))
	if !old.isHeapBig() || old.ref == a.ref || old.ref == b.ref || (*BigInt)(old.ref).hdr&bigSoleBit == 0 {
		return nil
	}
	return (*BigInt)(old.ref)
}

// jitLeafOwnRoom is own, jitLeafOwn's, if its room is n words.
func jitLeafOwnRoom(own *BigInt, n uintptr) *BigInt {
	if own == nil || uintptr(uint32(own.hdr>>bigCapShift)&bigLenMask) != n {
		return nil
	}
	return own
}

// jitBigLeafMake is jitBigLeaf's operator, of the call st describes.
func jitBigLeafMake(ctx *abi.Context, st *abi.GoCallState) uintptr {
	r := (*Runtime)(ctx.Host)
	if r == nil || r.meter != nil || r.jit == nil || st.Keep >= abi.MaxKeeps || len(st.Args) != 0 {
		return 0
	}
	op := st.Op
	sub, mul, div, mod := false, false, false, false
	switch ssa.GoOp(op & 0xff) {
	case ssa.GoAdd, ssa.GoAppend:
	case ssa.GoArith:
		switch ir.Operator(op >> abi.GoOpIndexShift) {
		case ir.Add:
		case ir.Sub:
			sub = true
		case ir.Mul:
			mul = true
		case ir.Div:
			div = true
		case ir.Mod:
			mod = true
		default:
			return 0
		}
	default:
		return 0
	}
	a := *(*Value)(unsafe.Pointer(&ctx.GoArgs[0]))
	b := *(*Value)(unsafe.Pointer(&ctx.GoArgs[1]))
	var aw, bw uint
	an, x, lx, ok := jitLeafParts(a, &aw)
	if !ok {
		return 0
	}
	bn, y, ly, ok := jitLeafParts(b, &bw)
	if !ok || !a.isHeapBig() && !b.isHeapBig() {
		// Of two held in their values: native code's, or Go's.
		return 0
	}
	if limit := uintptr(maxBigIntBits / bits.UintSize); mul && lx+ly > limit || !mul && max(lx, ly) >= limit {
		// A result that may pass maxBigIntBits: Go's call, which throws.
		return 0
	}
	f := r.bigFree
	if f == nil {
		f = new(bigFreeList)
		r.bigFree = f
	}
	var blk *BigInt
	var v Value
	var n, length uintptr
	var neg bool
	if div || mod {
		// By bignum.DivMod, as bigOp divides, but for a division by zero,
		// which throws: the quotient truncated, the remainder the
		// dividend's sign, in a block of its own length (bigOfKept) given
		// back, the value before's, or a new one.
		if ly == 0 {
			return 0
		}
		xs, ys := unsafe.Slice((*uint)(x), lx), unsafe.Slice((*uint)(y), ly)
		var mag []uint
		if div {
			mag, _ = bignum.DivMod(r.bigRoom(bignum.QuoRoom(len(xs), len(ys))), nil, xs, ys)
			neg = an != bn
		} else {
			_, mag = bignum.DivMod(nil, r.bigRoom(len(ys)), xs, ys)
			neg = an
		}
		v, blk = jitLeafPlace(ctx, op, a, b, r, f, neg, mag)
		goto made
	}
	if mul {
		if lx < ly {
			x, lx, y, ly = y, ly, x, lx
		}
		if ly == 0 {
			// A product by zero.
			v = shortBig(0)
			goto made
		}
		n = lx + ly
		if blk = jitLeafTake(f, n); blk == nil {
			if blk = jitLeafOwnRoom(jitLeafOwn(ctx, op, a, b), n); blk == nil {
				blk = jitLeafNew(r, n)
			}
		}
		z := unsafe.Add(unsafe.Pointer(blk), bigWordSize)
		length, neg = n, an != bn
		switch {
		case ly >= jitLeafKaratsuba:
			// By bignum's Mul or Sqr, as bigOp multiplies: Karatsuba's.
			zs, xs := unsafe.Slice((*uint)(z), n), unsafe.Slice((*uint)(x), lx)
			if a.sameBits(b) {
				zs = bignum.Sqr(zs, xs)
			} else {
				zs = bignum.Mul(zs, xs, unsafe.Slice((*uint)(y), ly))
			}
			length = uintptr(len(zs))
		case ly > 2 && lx > jitBigLeafMul:
			// A row a word of y, by bignum's kernels, as its schoolbook
			// multiplies.
			zs, xs := unsafe.Slice((*uint)(z), n), unsafe.Slice((*uint)(x), lx)
			ys := unsafe.Slice((*uint)(y), ly)
			zs[lx] = bignum.MulRow(zs[:lx], xs, ys[0])
			clear(zs[lx+1:])
			for j := 1; j < len(ys); j++ {
				if d := ys[j]; d != 0 {
					zs[int(lx)+j] = bignum.MulAddRow(zs[j:j+int(lx)], xs, d)
				}
			}
			if zs[n-1] == 0 {
				length--
			}
		default:
			jitLeafMul(z, x, lx, y, ly)
			if jitLeafWord(z, n-1) == 0 {
				length--
			}
		}
	} else {
		if sub {
			bn = !bn
		}
		if lx < ly {
			x, lx, an, y, ly, bn = y, ly, bn, x, lx, an
		}
		n = lx + 1
		if an == bn {
			if blk = jitLeafTake(f, n); blk == nil {
				if blk = jitLeafOwnRoom(jitLeafOwn(ctx, op, a, b), n); blk == nil {
					blk = jitLeafNew(r, n)
				}
			}
			z := unsafe.Add(unsafe.Pointer(blk), bigWordSize)
			c := jitLeafAdd(z, x, lx, y, ly)
			*(*uint)(unsafe.Add(z, lx*bigWordSize)) = c
			length, neg = lx+uintptr(c), an
		} else {
			// Of opposite signs: the larger magnitude's sign, the
			// difference.
			if lx == ly {
				switch jitLeafCmp(x, y, lx) {
				case 0:
					// Zero, held in its value.
					return 0
				case -1:
					x, an, y = y, bn, x
				}
			}
			if blk = jitLeafTake(f, n); blk == nil {
				// Not the value before's: a difference may shrink, and give
				// its block back.
				blk = jitLeafNew(r, n)
			}
			z := unsafe.Add(unsafe.Pointer(blk), bigWordSize)
			jitLeafSub(z, x, lx, y, ly)
			length, neg = lx, an
			for length > 0 && jitLeafWord(z, length-1) == 0 {
				length--
			}
			if length <= 64/bits.UintSize {
				// Held in its value: the block given back.
				w := uint64(0)
				if length == 1 {
					w = uint64(jitLeafWord(z, 0))
				}
				jitLeafGive(f, blk)
				blk, v = nil, shortBigMag(neg, w)
				goto made
			}
		}
	}
	blk.hdr = uint(length) | uint(n)<<bigCapShift
	if neg {
		blk.hdr |= bigNegBit
	}
	v = Value{num: math.Float64frombits(heapBigBits), ref: unsafe.Pointer(blk)}
made:
	// What bigRelease gives back, as the call's release says (ssa.CallSite's
	// Release, jitBigRelease).
	if rel := uint8(op >> abi.GoOpKindShift); rel != 0 {
		freshA, freshB := rel&ssa.ReleaseLeft != 0, rel&ssa.ReleaseRight != 0
		if freshA && a.isHeapBig() && a.ref != v.ref {
			jitLeafGive(f, (*BigInt)(a.ref))
		}
		if freshB && b.isHeapBig() && b.ref != v.ref && b.ref != a.ref {
			jitLeafGive(f, (*BigInt)(b.ref))
		}
		if rel&ssa.ReleaseOld != 0 {
			old := *(*Value)(unsafe.Pointer(&ctx.GoArgs[2]))
			if old.isHeapBig() && old.ref != v.ref && (*BigInt)(old.ref).hdr&bigSoleBit != 0 &&
				(!freshA || old.ref != a.ref) && (!freshB || old.ref != b.ref) {
				jitLeafGive(f, (*BigInt)(old.ref))
			}
			if blk != nil {
				blk.hdr |= bigSoleBit
			}
		}
	}
	*(*Value)(unsafe.Pointer(&ctx.Keep[st.Keep])) = v
	ctx.GoOp, ctx.GoKeep, ctx.GoState, ctx.GoStatus = op, st.Keep, 0, 0
	r.jit.bigLeafs++
	return ctx.GoResume
}

// jitLeafParts is v's sign and magnitude, its digits and their count, v a
// BigInt: one held in its value spelled in *w; false for any other value.
func jitLeafParts(v Value, w *uint) (bool, unsafe.Pointer, uintptr, bool) {
	bits64 := math.Float64bits(v.num)
	switch {
	case bits64 == heapBigBits:
		h := (*BigInt)(v.ref).hdr
		return h&bigNegBit != 0, unsafe.Add(v.ref, bigWordSize), uintptr(h & bigLenMask), true
	case bits64 >= shortIntBase:
		x := int64(uint64(uintptr(v.ref)-uintptr(unsafe.Pointer(&shortBigHigh)))<<shortBigLowBits | bits64&shortBigLowMask)
		neg := x < 0
		*w = uint(x)
		if neg {
			*w = uint(-x)
		}
		return neg, unsafe.Pointer(w), uintptr(min(*w, 1)), true
	case bits64 >= shortBigBase:
		// Past the int64 range, held in its value: 65 bits' two's
		// complement.
		off := uint64(uintptr(v.ref) - uintptr(unsafe.Pointer(&shortBigHighX)))
		lo := off<<shortBigLowBits | bits64&shortBigLowMask
		neg := off>>(64-shortBigLowBits) != 0
		*w = uint(lo)
		if neg {
			*w = uint(-lo)
		}
		return neg, unsafe.Pointer(w), 1, true
	}
	return false, nil, 0, false
}

// jitLeafWord is the i'th word at p.
func jitLeafWord(p unsafe.Pointer, i uintptr) uint {
	return *(*uint)(unsafe.Add(p, i*bigWordSize))
}

// jitLeafPlace is a quotient's or a remainder's value, of sign neg and
// magnitude mag, in scratch: held in its value where it fits; of two words,
// a new BigInt, as bigOf makes it; past them, in a block of its length
// given back, the value before's (jitLeafOwn), or a new one.
func jitLeafPlace(ctx *abi.Context, op uint64, a, b Value, r *Runtime, f *bigFreeList, neg bool, mag []uint) (Value, *BigInt) {
	n := uintptr(len(mag))
	if n < bigBlockFrom {
		v := bigOf(neg, mag)
		if v.isHeapBig() {
			return v, (*BigInt)(v.ref)
		}
		return v, nil
	}
	blk := jitLeafTake(f, n)
	if blk == nil {
		if blk = jitLeafOwnRoom(jitLeafOwn(ctx, op, a, b), n); blk == nil {
			blk = jitLeafNew(r, n)
		}
	}
	copy(unsafe.Slice((*uint)(unsafe.Add(unsafe.Pointer(blk), bigWordSize)), n), mag)
	blk.hdr = uint(n) | uint(n)<<bigCapShift
	if neg {
		blk.hdr |= bigNegBit
	}
	return Value{num: math.Float64frombits(heapBigBits), ref: unsafe.Pointer(blk)}, blk
}

// jitLeafNew is a new block of room for n words, where none was given
// back: what bigInPlace would allocate, counted (jitState's bigLeafNew).
func jitLeafNew(r *Runtime, n uintptr) *BigInt {
	r.jit.bigLeafNew++
	return newBigWords(int(n), false)
}

// jitLeafTake is bigTake: a block given back of room for n words, nil
// where there is none.
func jitLeafTake(f *bigFreeList, n uintptr) *BigInt {
	if n < bigBlockFrom || n > bigFreeTop {
		return nil
	}
	i := n - bigBlockFrom
	k := f.n[i]
	if k == 0 {
		return nil
	}
	k--
	f.n[i] = k
	b := f.b[i][k]
	f.b[i][k] = nil
	return b
}

// jitLeafGive is bigGive.
func jitLeafGive(f *bigFreeList, b *BigInt) {
	c := uintptr(uint32(b.hdr>>bigCapShift) & bigLenMask)
	if c < bigBlockFrom || c > bigFreeTop {
		return
	}
	i := c - bigBlockFrom
	if k := f.n[i]; k < bigFreeDepth {
		f.b[i][k] = b
		f.n[i] = k + 1
	}
}

// jitLeafAdd writes x+y, of lx and ly words, lx at least ly, to z's lx
// words, and returns the carry.
func jitLeafAdd(z, x unsafe.Pointer, lx uintptr, y unsafe.Pointer, ly uintptr) uint {
	var c uint
	i := uintptr(0)
	for ; i < ly; i++ {
		*(*uint)(unsafe.Add(z, i*bigWordSize)), c = bits.Add(jitLeafWord(x, i), jitLeafWord(y, i), c)
	}
	for ; i < lx; i++ {
		*(*uint)(unsafe.Add(z, i*bigWordSize)), c = bits.Add(jitLeafWord(x, i), 0, c)
	}
	return c
}

// jitLeafSub writes x-y, of lx and ly words, x at least y, to z's lx words.
func jitLeafSub(z, x unsafe.Pointer, lx uintptr, y unsafe.Pointer, ly uintptr) {
	var b uint
	i := uintptr(0)
	for ; i < ly; i++ {
		*(*uint)(unsafe.Add(z, i*bigWordSize)), b = bits.Sub(jitLeafWord(x, i), jitLeafWord(y, i), b)
	}
	for ; i < lx; i++ {
		*(*uint)(unsafe.Add(z, i*bigWordSize)), b = bits.Sub(jitLeafWord(x, i), 0, b)
	}
}

// jitLeafCmp compares x and y, of n words each: -1, 0 or 1.
func jitLeafCmp(x, y unsafe.Pointer, n uintptr) int {
	for i := n; i > 0; i-- {
		if a, b := jitLeafWord(x, i-1), jitLeafWord(y, i-1); a != b {
			if a < b {
				return -1
			}
			return 1
		}
	}
	return 0
}

// jitLeafMul writes x*y, of lx and ly words, lx at least ly, to z's lx+ly
// words, schoolbook, a row a word of y.
func jitLeafMul(z, x unsafe.Pointer, lx uintptr, y unsafe.Pointer, ly uintptr) {
	for j := uintptr(0); j < ly; j++ {
		d := jitLeafWord(y, j)
		var c uint
		for i := uintptr(0); i < lx; i++ {
			hi, lo := bits.Mul(jitLeafWord(x, i), d)
			at := unsafe.Add(z, (i+j)*bigWordSize)
			var k uint
			if j > 0 {
				lo, k = bits.Add(lo, *(*uint)(at), 0)
				hi += k
			}
			lo, k = bits.Add(lo, c, 0)
			*(*uint)(at), c = lo, hi+k
		}
		*(*uint)(unsafe.Add(z, (j+lx)*bigWordSize)) = c
	}
}
