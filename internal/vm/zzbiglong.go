package vm

import (
	"context"

	"github.com/go-quickjs/go-quickjs/internal/bignum"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// Long BigInt operations: a product, a quotient, a conversion to or from a
// string of operands of bignum.Long words or more, which can take seconds
// in one call, are made by bignum's checked entry points, which ask
// bigInterrupt at intervals of their work whether the host has stopped the
// script -- its context ended, its abort, the memory limit -- and stop if
// it has, as V8 stops its BigInt library's long operations. A shorter one
// is made as it was, nothing asked.

// bigCheck is what a long operation asks: bigInterrupt where the host can
// stop the script, nil where nothing can.
func (r *Runtime) bigCheck() func() error {
	if !r.bigStoppable() {
		return nil
	}
	return r.bigInterrupt
}

// bigStoppable reports whether the host can stop a long operation: a
// context that can end, an abort, a memory limit.
func (r *Runtime) bigStoppable() bool {
	return r.abort != nil || r.ctx != nil && r.ctx.Done() != nil || r.meter != nil
}

// bigInterrupt is checkInterruptNow without the debugger's turn, which may
// run script, and so other BigInt operations over the runtime's scratch,
// in the middle of one: why the host has stopped the script, or nil.
func (r *Runtime) bigInterrupt() error {
	if r.stopped != nil {
		return r.stopped
	}
	if r.abort != nil {
		select {
		case <-r.abort:
			return r.stop(context.Canceled)
		default:
		}
	}
	if r.ctx != nil {
		select {
		case <-r.ctx.Done():
			return r.stop(r.ctx.Err())
		default:
		}
	}
	if r.meter != nil {
		return r.checkMemory()
	}
	return nil
}

// bigMulLong is bigOp's product of x and y, together bignum.Long words or
// more, into blk's z, a square for sq.
func (r *Runtime) bigMulLong(blk *BigInt, z []uint, neg bool, x, y []uint, sq bool) (Value, error) {
	var err error
	if sq {
		z, err = bignum.SqrChecked(z, x, r.bigCheck())
	} else {
		z, err = bignum.MulChecked(z, x, y, r.bigCheck())
	}
	if err != nil {
		return Undefined, err
	}
	return bigDone(blk, neg, z), nil
}

// bigDivLong is bigOp's quotient or remainder, op's, of x, of bignum.Long
// words or more, by y, not zero.
func (r *Runtime) bigDivLong(op bytecode.Op, an bool, x []uint, bn bool, y []uint) (Value, error) {
	if op == bytecode.OpDiv {
		q, _, err := bignum.DivModChecked(r.bigRoom(bignum.QuoRoom(len(x), len(y))), nil, x, y, r.bigCheck())
		if err != nil {
			return Undefined, err
		}
		return r.bigOfKept(an != bn, q), nil
	}
	_, rem, err := bignum.DivModChecked(nil, r.bigRoom(len(y)), x, y, r.bigCheck())
	if err != nil {
		return Undefined, err
	}
	return r.bigOfKept(an, rem), nil
}

// bigPowLong is bigPow's squarings and products of a power of
// bignum.Long words or more, acc and sq its factors so far, t its scratch
// and e what is left of its exponent.
func (r *Runtime) bigPowLong(neg bool, acc, sq, t []uint, e uint64) (Value, error) {
	check := r.bigCheck()
	for {
		if e&1 != 0 {
			p, err := bignum.MulChecked(t[:len(acc)+len(sq)], acc, sq, check)
			if err != nil {
				return Undefined, err
			}
			acc = append(acc[:0], p...)
		}
		if e >>= 1; e == 0 {
			break
		}
		p, err := bignum.SqrChecked(t[:2*len(sq)], sq, check)
		if err != nil {
			return Undefined, err
		}
		sq = append(sq[:0], p...)
	}
	return bigOf(neg, acc), nil
}

// bigLong reports whether v is a BigInt of bignum.Long words or more.
func bigLong(v Value) bool {
	if !v.isHeapBig() {
		return false
	}
	_, x := heapParts(v)
	return len(x) >= bignum.Long
}

// bigStringOf is bigString, a long BigInt's digits written by bignum's
// checked conversion.
func (r *Runtime) bigStringOf(v Value, base int) (*String, error) {
	if !bigLong(v) {
		return bigString(v, base), nil
	}
	neg, x := heapParts(v)
	b, err := bignum.AppendChecked(nil, neg, x, base, r.bigCheck())
	if err != nil {
		return nil, err
	}
	return asciiString(b), nil
}

// stringToBigInt is StringToBigInt, a long string's digits read by
// bignum's checked conversion.
func (r *Runtime) stringToBigInt(s string) (*BigInt, bool, error) {
	if len(s) < bignum.Long {
		b, ok := StringToBigInt(s)
		return b, ok, nil
	}
	return stringToBigIntChecked(s, r.bigCheck())
}
