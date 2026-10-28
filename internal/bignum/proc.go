package bignum

// Interruption: an operation on large numbers can run for seconds in one
// call -- a product, a quotient, a conversion of millions of words -- which a
// runtime with a deadline, an abort or a memory limit must be able to stop,
// as V8's BigInt library lets V8 stop it (ProcessorImpl's work estimate and
// should_terminate). Each checked entry point (MulChecked, SqrChecked,
// DivModChecked, AppendChecked, ParseChecked) takes a check, which the
// operation asks, every checkWork word operations or so of its work, whether
// to stop; what it returns stops the operation, which returns it, its result
// undefined. The algorithms keep their account in a proc, nil for an
// operation no one can stop, which costs them nothing more than a nil check
// at steps of many words: an operation of fewer words than Long, its
// operands' and its result's together, is never checked, nor asked to be.

// Long is the words, an operation's operands' and its result's together,
// from which a caller that can be interrupted calls the checked entry
// points: below it, any operation takes some tens of microseconds at most,
// and the unchecked ones are what it costs now.
const Long = 256

// checkWork is the word operations -- a word's product added in, a word
// added, shifted or copied -- between two of a proc's checks: some
// milliseconds of work, as V8's kWorkEstimateThreshold is.
const checkWork = 1 << 22

// proc is an operation's account of its work, for one that can be
// interrupted: its check, the work since it was last asked, and what it
// said, which stops the operation -- each step that sees it returns at once,
// what it has written undefined.
type proc struct {
	check func() error
	work  int
	err   error
}

// newProc is the account of an operation with check, nil for none.
func newProc(check func() error) *proc {
	if check == nil {
		return nil
	}
	return &proc{check: check}
}

// spend adds n word operations, at most some millions, to p's account, and
// reports whether the operation is to stop: the check asked once the account
// passes checkWork. A nil p is never stopped.
func (p *proc) spend(n int) bool {
	if p == nil {
		return false
	}
	p.work += n
	if p.work >= checkWork {
		return p.poll()
	}
	return p.err != nil
}

// poll asks the check, its account emptied.
func (p *proc) poll() bool {
	p.work = 0
	if p.err == nil {
		p.err = p.check()
	}
	return p.err != nil
}

// stopped reports whether the operation is to stop.
func (p *proc) stopped() bool { return p != nil && p.err != nil }

// result is what an operation of p returns: nil, or why it stopped.
func (p *proc) result() error {
	if p == nil {
		return nil
	}
	return p.err
}

// MulChecked is Mul for a caller that can be interrupted: check, nil for
// none, is asked at intervals of the work, and what it returns stops the
// product and is returned, z's words then undefined.
func MulChecked(z, x, y []uint, check func() error) ([]uint, error) {
	p := newProc(check)
	if p == nil {
		return Mul(z, x, y), nil
	}
	if len(x) < len(y) {
		x, y = y, x
	}
	if len(y) == 0 {
		return z[:0], nil
	}
	z = mul(z[:len(x)+len(y)], x, y, p)
	if p.err != nil {
		return nil, p.err
	}
	return Norm(z), nil
}

// SqrChecked is Sqr for a caller that can be interrupted, as MulChecked is
// Mul.
func SqrChecked(z, x []uint, check func() error) ([]uint, error) {
	p := newProc(check)
	if p == nil || len(x) < karatsubaThreshold {
		return Sqr(z, x), nil
	}
	z = sqr(z[:2*len(x)], x, p)
	if p.err != nil {
		return nil, p.err
	}
	return Norm(z), nil
}

// DivModChecked is DivMod for a caller that can be interrupted, as
// MulChecked is Mul: q's and r's words undefined when it stops.
func DivModChecked(q, r, x, y []uint, check func() error) ([]uint, []uint, error) {
	p := newProc(check)
	q, r = divMod(q, r, x, y, p)
	if err := p.result(); err != nil {
		return nil, nil, err
	}
	return q, r, nil
}

// AppendChecked is Append for a caller that can be interrupted, as
// MulChecked is Mul: dst's appended bytes undefined when it stops.
func AppendChecked(dst []byte, neg bool, x []uint, base int, check func() error) ([]byte, error) {
	p := newProc(check)
	dst = appendNat(dst, neg, x, base, p)
	if err := p.result(); err != nil {
		return nil, err
	}
	return dst, nil
}

// ParseChecked is Parse for a caller that can be interrupted, as MulChecked
// is Mul.
func ParseChecked(s string, base int, check func() error) ([]uint, bool, error) {
	p := newProc(check)
	z, ok := parse(s, base, p)
	if err := p.result(); err != nil {
		return nil, false, err
	}
	return z, ok, nil
}
