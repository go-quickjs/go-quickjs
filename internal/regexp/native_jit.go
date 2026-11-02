//go:build quickjs_jit

package regexp

import (
	"runtime"
	"sync/atomic"
	"unsafe"
)

// Native code for a pattern, as V8's Irregexp makes it.
//
// A runtime with the JIT gives the patterns it compiles a host (UseNative).
// Once a pattern has been matched nativeAfter times over subjects of one
// kind -- an ASCII string's bytes, or a string's UTF-16 code units -- its
// program is compiled for that kind to machine code (native_gen.go),
// placed in the runtime's arena, and every later search of that kind's
// matcher runs that code rather than the matcher's search: the attempts and
// the search around them -- the first units, the advance, sticky -- as
// Irregexp's code is the whole match. The matcher over UTF-8, for patterns
// Go programs share between goroutines (Concurrent), runs none.
//
// The code returns to Go when it matches, when it fails, when its stack is
// full -- Go grows it and enters again where it left off -- when its steps
// run out, which they do at the next check the matcher would make, at the
// end of the budget, or after nativePoll steps: native code is never a safe
// point, and goes back to Go that often so that the goroutine can be
// preempted -- and when the search has passed where the required literal
// was found, for Go to look for it again.

const nativeBuilt = true

// NativeHost is where a runtime puts its patterns' native code: its JIT's
// arena, and the budget the JIT holds its code in.
type NativeHost struct {
	// Place publishes code executable -- in the runtime's arena, as
	// jit.PlaceRaw does -- or fails, and the pattern is matched by the
	// matcher.
	Place func(code []byte) (NativeCode, error)
	// Room reports whether n more bytes of code fit the budget, making room
	// for them if it can.
	Room func(n int) bool
	// After is how many matches of one kind of subject a pattern has before
	// its code for that kind is made, or 0 for nativeAfter.
	After int32
	// held is what the patterns' code takes: an allocation of its own, so
	// that a pattern's cleanup, which counts its code out, reaches nothing
	// of the host's -- whose functions hold the runtime, which holds the
	// pattern, which a cleanup's argument must not reach, or it is never
	// collected.
	held *atomic.Int64
}

// NativeListing is the code a pattern's program compiles to for arch,
// amd64 or arm64 -- on any machine -- over UTF-16 units where wide is set and
// else over ASCII bytes, and how many of its bytes are instructions, the
// rest its tables; or false where the program is not one native code is
// made for. It is for internal/jit/verify, which disassembles the code.
func NativeListing(pattern, flags, arch string, wide bool) ([]byte, int, bool) {
	re, err := Compile(pattern, flags)
	if err != nil {
		return nil, 0, false
	}
	p, ok := genNative(arch, re.prog, 2*(re.groupCount+1), wide, re.flags&FlagUnicode != 0)
	return p.bytes, p.text, ok
}

// NativeCode is code a host placed (jit.RawCode).
type NativeCode interface {
	// Enter runs the code from its start with ctx.
	Enter(ctx unsafe.Pointer)
	// Size is the memory it takes.
	Size() int
	// Close gives the memory back.
	Close() error
}

// maxNativeBytes bounds a pattern's code, as jit.MaxCodeBytes bounds a
// placement.
const maxNativeBytes = 1 << 20

// Held is the memory the host's patterns' code takes, which goes down as
// patterns are collected (on another goroutine).
func (h *NativeHost) Held() int {
	if h.held == nil {
		return 0
	}
	return int(h.held.Load())
}

// UseNative has the pattern matched by native code, placed with h, once it
// has been matched often enough. A pattern goroutines match at once
// (Concurrent) is not, nor one compiled for UTF-8, nor one on a machine the
// code is not made for.
func (re *Regexp) UseNative(h *NativeHost) {
	if re.native != nil || re.concurrent || re.utf8 || newMasm(runtime.GOARCH) == nil {
		return
	}
	if h.held == nil {
		h.held = new(atomic.Int64)
	}
	re.native = &nativeSlot{host: h}
}

// HasNative reports whether the pattern has native code for ASCII subjects.
func (re *Regexp) HasNative() bool { return re.native != nil && re.native.ascii.code != nil }

// HasNativeUTF16 reports whether the pattern has native code for subjects
// of UTF-16 code units.
func (re *Regexp) HasNativeUTF16() bool { return re.native != nil && re.native.wide.code != nil }

// nativeAfter is how many matches of one kind of subject a pattern has
// before its code for it is made, unless its host says: V8 interprets a
// pattern's first match and compiles it for its second.
const nativeAfter = 2

// nativeRetry is how many more matches a pattern the budget had no room for
// waits before it asks again.
const nativeRetry = 64

// nativePoll is the most steps native code takes before it goes back to Go.
const nativePoll = 1 << 16

// nativeSlot is a pattern's native code for each kind of subject, or what
// decides when it is made.
type nativeSlot struct {
	host  *NativeHost
	ascii nativeVariant
	wide  nativeVariant
}

// nativeVariant is the code for one kind of subject.
type nativeVariant struct {
	code *nativeCode
	runs int32
	// size is the bytes of the code the budget last had no room for, which
	// is asked for again before the code is made again: the same program
	// makes the same code.
	size    int32
	refused bool
}

// nativeCode is a pattern's code, placed. It is its cleanup's argument, and
// so holds nothing that holds the pattern.
type nativeCode struct {
	raw  NativeCode
	held *atomic.Int64
	// maxPush is the most words one instruction pushes, which the stack
	// keeps room for past its limit.
	maxPush int
	// shift is log2 of a unit's bytes: 0 over bytes, 1 over UTF-16.
	shift uint
}

// release gives the code back once the pattern is collected.
func (c *nativeCode) release() {
	size := c.raw.Size()
	if c.raw.Close() == nil {
		c.held.Add(-int64(size))
	}
}

// nativeTick counts a match of the pattern over an ASCII subject, or with
// wide over UTF-16, making its code for that kind at the nativeAfter'th.
func (re *Regexp) nativeTick(wide bool) {
	n := re.native
	v := &n.ascii
	if wide {
		v = &n.wide
	}
	if v.code != nil || v.refused {
		return
	}
	after := n.host.After
	if after <= 0 {
		after = nativeAfter
	}
	if v.runs++; v.runs < after {
		return
	}
	// Code the budget had no room for is not made again until there is
	// room: made at every retry and thrown away, a large pattern's code --
	// tens of milliseconds of it -- had made its matches 75 times slower
	// than the matcher's in a runtime with a small JIT budget.
	if v.size > 0 && !n.host.Room(int(v.size)) {
		v.runs = after - nativeRetry
		return
	}
	prog, ok := genNative(runtime.GOARCH, re.prog, 2*(re.groupCount+1), wide, re.flags&FlagUnicode != 0)
	if !ok {
		v.refused = true
		return
	}
	if !n.host.Room(len(prog.bytes)) {
		v.runs, v.size = after-nativeRetry, int32(len(prog.bytes))
		return
	}
	raw, err := n.host.Place(prog.bytes)
	if err != nil || raw == nil {
		v.refused = true
		return
	}
	c := &nativeCode{raw: raw, held: n.host.held, maxPush: prog.maxPush}
	if wide {
		c.shift = 1
	}
	n.host.held.Add(int64(raw.Size()))
	runtime.AddCleanup(re.core, (*nativeCode).release, c)
	v.code = c
}

// attempt is the match of the pattern at pos, by the matcher: a pattern
// with native code for the subject's kind has it search (searchNative).
func (m *matcherASCII) attempt(re *Regexp, pos int) (bool, error) { return m.run(re.prog.code, pos) }

// attempt is the match of the pattern at pos, by the matcher.
func (m *matcher) attempt(re *Regexp, pos int) (bool, error) { return m.run(re.prog.code, pos) }

// attempt is the match of the pattern at pos, by the matcher.
func (m *matcherUTF8) attempt(re *Regexp, pos int) (bool, error) { return m.run(re.prog.code, pos) }

// searchNative is search from start done by the pattern's native code for
// ASCII subjects, sticky where it is to try start alone, with lit the
// required literal search looks for; done is false where there is no code.
func (m *matcherASCII) searchNative(re *Regexp, dst []int, start int, sticky bool, lit *requiredLit) (caps []int, done bool, err error) {
	c := re.native.ascii.code
	units := m.in.units
	// The code reads the subject from start: a start outside it is the
	// matcher's to refuse.
	if c == nil || uint(start) > uint(len(units)) {
		return nil, false, nil
	}
	litAt := 0
	if lit != nil {
		if litAt = indexUnitsASCII(units, &lit.literal, start+lit.minOff); litAt < 0 {
			return nil, true, nil
		}
	}
	r := m.native
	if r == nil {
		r = new(nativeRun)
		m.native = r
	}
	r.begin(c, unsafe.Pointer(unsafe.SliceData(units)), len(units), m.caps, m.counters, m.emptyMarks, start, sticky, litAt)
	for {
		ok, err := r.run(c, &m.steps, &m.checkAt, m.check)
		if err != nil {
			return nil, true, err
		}
		if ok {
			return append(dst[:0], m.caps...), true, nil
		}
		if r.ctx.status == nativeFailed {
			return nil, true, nil
		}
		// The search has passed where the literal was: it is looked for
		// again.
		if litAt = indexUnitsASCII(units, &lit.literal, int(r.ctx.cur)+lit.minOff); litAt < 0 {
			return nil, true, nil
		}
		r.ctx.litAt = int64(litAt)
	}
}

// searchNative is search from start done by the pattern's native code for
// UTF-16 subjects, as matcherASCII's is for ASCII ones.
func (m *matcher) searchNative(re *Regexp, dst []int, start int, sticky bool, lit *requiredLit) (caps []int, done bool, err error) {
	c := re.native.wide.code
	units := m.in.units
	// The code reads the subject from start: a start outside it is the
	// matcher's to refuse.
	if c == nil || uint(start) > uint(len(units)) {
		return nil, false, nil
	}
	litAt := 0
	if lit != nil {
		if litAt = indexUnits(units, &lit.literal, start+lit.minOff); litAt < 0 {
			return nil, true, nil
		}
	}
	r := m.native
	if r == nil {
		r = new(nativeRun)
		m.native = r
	}
	r.begin(c, unsafe.Pointer(unsafe.SliceData(units)), 2*len(units), m.caps, m.counters, m.emptyMarks, start, sticky, litAt)
	for {
		ok, err := r.run(c, &m.steps, &m.checkAt, m.check)
		if err != nil {
			return nil, true, err
		}
		if ok {
			return append(dst[:0], m.caps...), true, nil
		}
		if r.ctx.status == nativeFailed {
			return nil, true, nil
		}
		if litAt = indexUnits(units, &lit.literal, int(r.ctx.cur>>1)+lit.minOff); litAt < 0 {
			return nil, true, nil
		}
		r.ctx.litAt = int64(litAt) << 1
	}
}

// searchNative is search over UTF-8, which runs no native code.
func (m *matcherUTF8) searchNative(*Regexp, []int, int, bool, *requiredLit) ([]int, bool, error) {
	return nil, false, nil
}

// nativeCtx is what native code runs with: the addresses it reads and
// writes, and where it is, which it writes back when it returns. Every
// field is a word, and none is a pointer Go sees: what they address the
// matcher holds.
type nativeCtx struct {
	subject, end uintptr
	caps         uintptr
	counters     uintptr
	marks        uintptr
	// stackBase is the backtracking stack's bottom, and stackLimit the
	// highest top an instruction may begin at, which leaves room for the
	// most words one pushes.
	stackBase, stackLimit uintptr
	// top is the stack's top and cur the position, as offsets in bytes from
	// the stack's bottom and the subject's start; left is the steps the
	// code may yet take, less one.
	top, cur, left int64
	// resume is where the code goes on when entered, or 0 at the start of
	// a search.
	resume uintptr
	// status says why the code returned.
	status int64
	// sticky is not 0 where the search tries its start alone, and litAt is
	// where the required literal was found last, an offset in bytes.
	sticky, litAt int64
}

// The context's fields' offsets, for the code.
const (
	offSubject    = int32(unsafe.Offsetof(nativeCtx{}.subject))
	offEnd        = int32(unsafe.Offsetof(nativeCtx{}.end))
	offCaps       = int32(unsafe.Offsetof(nativeCtx{}.caps))
	offCounters   = int32(unsafe.Offsetof(nativeCtx{}.counters))
	offMarks      = int32(unsafe.Offsetof(nativeCtx{}.marks))
	offStackBase  = int32(unsafe.Offsetof(nativeCtx{}.stackBase))
	offStackLimit = int32(unsafe.Offsetof(nativeCtx{}.stackLimit))
	offTop        = int32(unsafe.Offsetof(nativeCtx{}.top))
	offCur        = int32(unsafe.Offsetof(nativeCtx{}.cur))
	offLeft       = int32(unsafe.Offsetof(nativeCtx{}.left))
	offResume     = int32(unsafe.Offsetof(nativeCtx{}.resume))
	offStatus     = int32(unsafe.Offsetof(nativeCtx{}.status))
	offSticky     = int32(unsafe.Offsetof(nativeCtx{}.sticky))
	offLitAt      = int32(unsafe.Offsetof(nativeCtx{}.litAt))
)

// Why native code returned.
const (
	nativeFailed = iota
	nativeMatched
	// nativeLimit: its steps ran out.
	nativeLimit
	// nativeGrow: its stack is full.
	nativeGrow
	// nativeLit: the search has passed where the required literal was.
	nativeLit
)

// nativeRun is a matcher's state for native code: the context, and the
// backtracking stack, which grows to fit the deepest match.
type nativeRun struct {
	ctx   nativeCtx
	stack []uint64
}

// large reports whether the stack has grown past what a lent matcher keeps.
func (r *nativeRun) large() bool { return r != nil && len(r.stack) > 4*maxLentFrames }

// setStack points the context at the stack.
func (r *nativeRun) setStack(c *nativeCode) {
	x := &r.ctx
	x.stackBase = uintptr(unsafe.Pointer(unsafe.SliceData(r.stack)))
	x.stackLimit = x.stackBase + uintptr(len(r.stack)-c.maxPush)*8
}

// begin readies the context for a search from start by the pattern's code
// c, over the n bytes of the subject at subject, with the matcher's
// captures, counters and marks, sticky or not, with the required literal
// found at litAt; the stack is made when first needed.
func (r *nativeRun) begin(c *nativeCode, subject unsafe.Pointer, n int, caps, counters, marks []int,
	start int, sticky bool, litAt int) {
	if len(r.stack) < c.maxPush+16 {
		r.stack = make([]uint64, max(4*maxLentFrames, 2*(c.maxPush+16)))
	}
	r.setStack(c)
	x := &r.ctx
	x.subject = uintptr(subject)
	x.end = x.subject + uintptr(n)
	x.caps = uintptr(unsafe.Pointer(unsafe.SliceData(caps)))
	x.counters = uintptr(unsafe.Pointer(unsafe.SliceData(counters)))
	x.marks = uintptr(unsafe.Pointer(unsafe.SliceData(marks)))
	x.top, x.cur, x.resume = 0, int64(start)<<c.shift, 0
	x.sticky, x.litAt = 0, int64(litAt)<<c.shift
	if sticky {
		x.sticky = 1
	}
}

// run enters the code until it matches, fails, or the search needs the
// required literal looked for again -- ctx.status says which of the last
// two -- with the matcher's steps and check; or until the check or the
// budget stops it. The matcher holds what the context addresses.
func (r *nativeRun) run(c *nativeCode, steps, checkAt *int, check func() error) (bool, error) {
	x := &r.ctx
	for {
		limit := nativeStepLimit(*steps, *checkAt, check)
		x.left = int64(limit - *steps)
		c.raw.Enter(unsafe.Pointer(x))
		*steps = limit - int(x.left)
		switch x.status {
		case nativeMatched:
			return true, nil
		case nativeFailed, nativeLit:
			return false, nil
		case nativeLimit:
			if err := nativeBudget(*steps, checkAt, check); err != nil {
				return false, err
			}
		case nativeGrow:
			s := make([]uint64, 2*len(r.stack)+c.maxPush)
			copy(s, r.stack[:x.top/8])
			r.stack = s
			r.setStack(c)
		}
	}
}

// nativeStepLimit is the step count past which native code goes back to Go:
// where the matcher would next ask its check or find its budget spent, or
// nativePoll steps on.
func nativeStepLimit(steps, checkAt int, check func() error) int {
	limit := steps + nativePoll
	if check != nil {
		return min(limit, checkAt-1)
	}
	return min(limit, maxSteps)
}

// nativeBudget is what run does at an instruction past the limit: it asks
// the check, or fails for want of steps.
func nativeBudget(steps int, checkAt *int, check func() error) error {
	if check != nil {
		if steps >= *checkAt {
			*checkAt = steps + checkInterval
			return check()
		}
		return nil
	}
	if steps > maxSteps {
		return ErrComplexity
	}
	return nil
}
