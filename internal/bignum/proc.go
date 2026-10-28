package bignum

import "sync"

// Interruption: an operation on large numbers can run for seconds in one
// call -- a product, a quotient, a conversion of millions of words -- which a
// runtime with a deadline, an abort or a memory limit must be able to stop,
// as V8's BigInt library lets V8 stop it (ProcessorImpl's work estimate and
// should_terminate). Each checked entry point (MulChecked, SqrChecked,
// DivModChecked, AppendChecked, ParseChecked) takes a check, which the
// operation asks, every checkWork word operations or so of its work, whether
// to stop; what it returns stops the operation, which returns it, its result
// undefined. The algorithms keep their account in a proc, which costs them
// an addition at steps of many words: an operation of fewer words than
// Long, its operands' and its result's together, is never checked, nor
// asked to be.

// Long is the words, an operation's operands' and its result's together,
// from which a caller that can be interrupted calls the checked entry
// points: below it, any operation takes some tens of microseconds at most,
// and the unchecked ones are what it costs now.
const Long = 256

// checkWork is the word operations -- a word's product added in, a word
// added, shifted or copied -- between two of a proc's checks: some
// milliseconds of work, as V8's kWorkEstimateThreshold is.
const checkWork = 1 << 22

// proc is a large operation's state: its account of its work, for one
// that can be interrupted -- its check, nil for none, the work since it was
// last asked, and what it said, which stops the operation, each step that
// sees it returning at once, what it has written undefined -- and its
// scratch, a stack the steps take their room from and give it back to, as
// math/big's does, so that a product of thousands of steps allocates once
// or twice, not at each.
type proc struct {
	check func() error
	work  int
	err   error
	stk   []uint
	top   int
}

// procs are procs done with, their stacks kept for the next operation's,
// so that one of numbers of a few thousand words allocates nothing.
var procs = sync.Pool{New: func() any { return new(proc) }}

// maxKept is the most words of stack a proc is kept with.
const maxKept = 1 << 18

// newProc is the state of an operation with check, nil for none, which
// done ends.
func newProc(check func() error) *proc {
	p := procs.Get().(*proc)
	p.check, p.work, p.err, p.top = check, 0, nil, 0
	return p
}

// done ends p's operation, its stack kept for another's, and returns why
// it stopped, or nil.
func (p *proc) done() error {
	err := p.err
	p.check, p.err = nil, nil
	if len(p.stk) > maxKept {
		p.stk = nil
	}
	procs.Put(p)
	return err
}

// spend adds n word operations, at most some millions, to p's account, and
// reports whether the operation is to stop: the check asked once the account
// passes checkWork.
func (p *proc) spend(n int) bool {
	p.work += n
	if p.work >= checkWork {
		return p.poll()
	}
	return p.err != nil
}

// poll asks the check, if there is one, its account emptied.
func (p *proc) poll() bool {
	p.work = 0
	if p.err == nil && p.check != nil {
		p.err = p.check()
	}
	return p.err != nil
}

// stopped reports whether the operation is to stop.
func (p *proc) stopped() bool { return p.err != nil }

// alloc is n words of scratch, not cleared, until the stack is given back
// to below them (release): a new stack, twice the size, where the one in
// use is full, the room handed out of the old one kept by those it was
// handed to.
func (p *proc) alloc(n int) []uint {
	if p.top+n > len(p.stk) {
		p.stk = make([]uint, max(2*len(p.stk), p.top+n, 1024))
	}
	s := p.stk[p.top : p.top+n : p.top+n]
	p.top += n
	return s
}

// mark is where the stack is in use to, for release.
func (p *proc) mark() int { return p.top }

// release gives back the scratch handed out since mark m.
func (p *proc) release(m int) { p.top = m }

// MulChecked is Mul for a caller that can be interrupted: check, nil for
// none, is asked at intervals of the work, and what it returns stops the
// product and is returned, z's words then undefined.
func MulChecked(z, x, y []uint, check func() error) ([]uint, error) {
	if check == nil {
		return Mul(z, x, y), nil
	}
	if len(x) < len(y) {
		x, y = y, x
	}
	if len(y) == 0 {
		return z[:0], nil
	}
	p := newProc(check)
	z = mul(z[:len(x)+len(y)], x, y, p)
	if err := p.done(); err != nil {
		return nil, err
	}
	return Norm(z), nil
}

// SqrChecked is Sqr for a caller that can be interrupted, as MulChecked is
// Mul.
func SqrChecked(z, x []uint, check func() error) ([]uint, error) {
	if check == nil || len(x) < karatsubaThreshold {
		return Sqr(z, x), nil
	}
	p := newProc(check)
	z = sqr(z[:2*len(x)], x, p)
	if err := p.done(); err != nil {
		return nil, err
	}
	return Norm(z), nil
}

// DivModChecked is DivMod for a caller that can be interrupted, as
// MulChecked is Mul: q's and r's words undefined when it stops.
func DivModChecked(q, r, x, y []uint, check func() error) ([]uint, []uint, error) {
	p := newProc(check)
	q, r = divMod(q, r, x, y, p)
	if err := p.done(); err != nil {
		return nil, nil, err
	}
	return q, r, nil
}

// AppendChecked is Append for a caller that can be interrupted, as
// MulChecked is Mul: dst's appended bytes undefined when it stops.
func AppendChecked(dst []byte, neg bool, x []uint, base int, check func() error) ([]byte, error) {
	p := newProc(check)
	dst = appendNat(dst, neg, x, base, p)
	if err := p.done(); err != nil {
		return nil, err
	}
	return dst, nil
}

// ParseChecked is Parse for a caller that can be interrupted, as MulChecked
// is Mul.
func ParseChecked(s string, base int, check func() error) ([]uint, bool, error) {
	p := newProc(check)
	z, ok := parse(s, base, p)
	if err := p.done(); err != nil {
		return nil, false, err
	}
	return z, ok, nil
}
