package vm

import (
	"strings"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/regexp"
)

// A string's replace of a RegExp by a string whose result is read only
// for its length, s.replace(re, '').length -- as every replace of
// Octane's RegExp is, and a count of what a pattern does not match is
// written -- makes the matches, the legacy statics and lastIndex as any
// other, and the length of the string it would have made, not the string:
// the subject's length less the matches' and plus the template's for each.
// The string was a copy of the subject's pieces, an allocation of the
// subject's size: a replace of 14 matches in 4 KB was half copying.
//
// Whether a call's result is only measured the bytecode says: a get_length
// right after it, in the same block. The tree tier builds the two into one
// node (replaceLenCall), for a call of a method named replace given two
// arguments on a receiver of any kind deadExecCall takes; the node tells
// String.prototype.replace's function (replaceDead's measure, as its dead
// is told a result nothing reads) that the call about to be made is of a
// result only measured, of that RegExp. Where the call is the built-in's
// direct replace (replaceDirect) of a string by a template without a $,
// whose result could not be too long to be a string, the function gives
// back the length, a number, which the node answers; anything else it
// replaces as before, and the node measures what it gives back as the
// get_length would have (tctx's getLength). The call is made as any other,
// through callRest: its tick, its frame, its errors. Native code tells the
// function nothing.
//
// The functions are generic, each instantiated with struct{} alone, for
// the reason zzz_regexpdead.go's are; the tree builder reaches
// replaceLenCall through opHook (zzz_typeof.go).

// replaceDeadOfRuntime is the realm's replace function's wrapper,
// replaceDeadOf's, which r.strReplaceDead points into.
func replaceDeadOfRuntime[_ any](r *Runtime) *replaceDead[struct{}] {
	return (*replaceDead[struct{}])(unsafe.Add(unsafe.Pointer(r.strReplaceDead), -int(unsafe.Offsetof(replaceDead[struct{}]{}.dead))))
}

// replaceMeasured is regExpReplace's replace of s by rx, whose flags are fl,
// with the template tv, which has no $, directly (replaceDirect), where the
// result is only measured: lastIndex, the matches and the legacy statics as
// it makes them, and the length of the string it would make. It reports
// false, having done nothing, where the replacement could be too long to be
// a string, which regExpReplace throws for.
func replaceMeasured[_ any](r *Runtime, rx Value, s, tv *String, fl regexp.Flags) (int, bool, error) {
	n, per := int64(s.length), int64(tv.length)
	if n+(n+1)*per > maxStringLength {
		return 0, false, nil
	}
	if fl&regexp.FlagGlobal == 0 {
		// One match, from lastIndex where the RegExp is sticky.
		if err := r.tick(); err != nil {
			return 0, true, err
		}
		caps, err := r.execMatch(rx, s)
		if err != nil || caps == nil {
			return s.length, true, err
		}
		return s.length - (caps[1] - caps[0]) + tv.length, true, nil
	}
	if err := r.setLastIndex(rx.Object(), Int(0)); err != nil {
		return 0, true, err
	}
	room := r.takeSpans()
	var err error
	if s.ascii {
		room, err = execMatchAllASCII[struct{}](r, rx, s, room)
	} else {
		room, err = r.execMatchAll(rx, s, fl&(regexp.FlagUnicode|regexp.FlagUnicodeSets) != 0, room)
	}
	length := s.length
	if err == nil {
		width := 2 * (rx.Object().dataOf().(*regexpData).re.GroupCount() + 1)
		for k := 0; k+1 < len(room); k += width {
			length += tv.length - (room[k+1] - room[k])
		}
	}
	r.giveSpans(room)
	return length, true, err
}

// measuredCall is replaceDead's call where its measure is the RegExp
// args[0] holds: the length, a number, of the string the replace would
// make, where it is the built-in's direct replace of a string by a
// template without a $ (replaceMeasured), and ok; not ok, having done
// nothing, for any other.
func measuredCall[_ any](r *Runtime, this Value, args []Value) (Value, bool, error) {
	if len(args) < 2 || !this.IsString() || !args[1].IsString() || r.meter != nil {
		return Undefined, false, nil
	}
	tv := args[1].String()
	if strings.IndexByte(tv.Go(), '$') >= 0 {
		return Undefined, false, nil
	}
	fl, ok := r.replaceDirect(args[0])
	if !ok {
		return Undefined, false, nil
	}
	n, ok, err := replaceMeasured[struct{}](r, args[0], this.String(), tv, fl)
	if !ok {
		return Undefined, false, nil
	}
	return Int(n), true, err
}

// measuredFrameless is a measured replace's call (measuredCall) made
// without its frames -- replace's and the exec's its matches are made in --
// as a leaf's call is, where its RegExp is global, a realm's as the realm
// made it, its prototype pristine (armPristine) and its lastIndex
// writable, and its subject and its template, which has no $, are ASCII:
// then nothing it does runs a script. It ticks as the call would; it
// reports false where a match or a tick fails, having written lastIndex 0
// at most, as the call does first, and the call is made again with its
// frames, which throws.
func measuredFrameless[_ any](r *Runtime, this Value, args []Value) (int, bool) {
	if len(args) < 2 || !this.IsString() || !args[1].IsString() || !args[0].IsObject() || r.meter != nil || r.debug != nil {
		return 0, false
	}
	s, tv, o := this.String(), args[1].String(), args[0].Object()
	p := r.proto.regexp
	if !s.ascii || !tv.ascii || o.class != ClassRegExp || o.proto != p || p == nil || p.shape == nil || !p.shape.pristine ||
		len(o.vals) != 1 || o.shape.keys[0].key != atomLastIndex || o.shape.keys[0].flags&propWritable == 0 {
		return 0, false
	}
	d, ok := o.dataOf().(*regexpData)
	if !ok || d.re.Flags()&regexp.FlagGlobal == 0 || strings.IndexByte(tv.Go(), '$') >= 0 {
		return 0, false
	}
	if n := int64(s.length); n+(n+1)*int64(tv.length) > maxStringLength {
		return 0, false
	}
	if err := r.tick(); err != nil {
		return 0, false
	}
	o.vals[0] = Int(0)
	var check func() error
	if r.abort != nil || r.ctx != nil && r.ctx.Done() != nil {
		check = r.checkInterruptNow
	}
	spans := r.takeSpans()
	spans, next, err := regexp.MatchAllASCIIInto[struct{}](d.re, spans, s.Go(), check, r.tick)
	if err != nil {
		r.giveSpans(spans)
		return 0, false
	}
	if next < 0 {
		next = 0
	}
	o.vals[0] = Int(next)
	width := 2 * (d.re.GroupCount() + 1)
	length := s.length
	for k := 0; k+1 < len(spans); k += width {
		length += tv.length - (spans[k+1] - spans[k])
	}
	if len(spans) > 0 {
		r.recordLegacyMatch(d.legacy(), s, spans[len(spans)-width:])
	}
	r.giveSpans(spans)
	return length, true
}

// replaceLen is a node of a call of replace whose result is only measured,
// and the get_length after it: deadExec's, its fields as deadExec's.
type replaceLen[_ any] struct {
	rv, f  tval
	args   []tval
	p, pc  int
	k      uint32
	argLit bool
}

// node is the call of the method f reads, on the receiver rv evaluates --
// or, rv nil, the one in slot p-1 -- and its result's length: the
// realm's replace told that its result is only measured, anything else
// called as callOperands's node calls a method, and what it gives back
// measured as the get_length at pc+1 measures it. A RegExp literal given
// to replace is given back as regexpArgCall's node gives it.
func (d *replaceLen[_]) node(c *tctx) Value {
	var recv Value
	if d.rv != nil {
		recv = d.rv(c)
	}
	fv := d.f(c)
	p := d.p
	for i, a := range d.args {
		c.stack[p+1+i] = a(c)
	}
	c.at(d.pc)
	if d.rv == nil {
		recv = c.stack[p-1]
	}
	r := c.r
	as := c.stack[p+1 : p+1+len(d.args)]
	var v Value
	if fv.IsObject() && fv.Object() == r.strReplaceFn && r.strReplaceDead != nil {
		o := fv.Object()
		rx := as[0]
		done := d.argLit && rx.IsObject() && r.regexpArgCall(o, recv, as)
		if n, ok := measuredFrameless[struct{}](r, recv, as); ok {
			if done {
				r.regexpLiteralDone(&c.cl.fn.Constants[d.k], rx.Object())
			}
			return Int(n)
		}
		var err error
		if rx.IsObject() && r.debug == nil {
			l := replaceDeadOfRuntime[struct{}](r)
			l.measure = rx.Object()
			v, err = r.callRest(o, o.fn(), recv, as)
			l.measure = nil
		} else {
			v, err = r.callRest(o, o.fn(), recv, as)
		}
		if err != nil {
			c.throw(err)
		}
		if done {
			r.regexpLiteralDone(&c.cl.fn.Constants[d.k], rx.Object())
		}
		if v.IsNumber() {
			// The length the replace measured.
			return v
		}
	} else {
		v = regexpCallMethod(c, fv, recv, as)
	}
	c.at(d.pc + 1)
	if v.IsString() {
		return Int(v.String().Len())
	}
	return c.getLength(v, d.pc+1)
}

// replaceLenCall builds the call_method at pc, of replace's read, given
// two arguments, and the get_length after it into replaceLen's node, and
// reports whether it did: the get_length is then skipped.
func replaceLenCall[_ any](b *tbuilder, pc int, code []bytecode.Instr, more bool) bool {
	fn := b.fn
	n := int(code[pc].A)
	k := len(b.stack) - n - 1
	if !more || fn.Generator || fn.Async || n != 2 || k < 1 || code[pc+1].Op != bytecode.OpGetLength {
		return false
	}
	m, recv := b.stack[k], b.stack[k-1]
	// replace's first argument a RegExp literal's, whose constant it is.
	argLit := b.stack[k+1].regexp && b.stack[k+1].slot < 0
	d := &replaceLen[struct{}]{p: k, pc: pc}
	switch {
	case m.method != nil:
		// Given a literal, regexpArgMethodCall's, which gives it back.
		if int(m.method.in.A) >= len(fn.Names) || m.v == nil || fn.Names[m.method.in.A] != "replace" || argLit {
			return false
		}
		if (recv.local || recv.this) && recv.v == nil && recv.slot < 0 {
			d.rv = recv.tree()
		} else if propIn(recv) != nil {
			d.rv = recv.v
		} else {
			return false
		}
	case m.rare == replaceDeadRare && m.v != nil && m.slot < 0 && int(recv.slot) == k-1:
		if argLit {
			d.argLit, d.k = true, b.stack[k+1].k
		}
	default:
		return false
	}
	d.args = make([]tval, n)
	for i := n - 1; i >= 0; i-- {
		d.args[i] = b.pop().tree()
	}
	d.f = b.pop().tree()
	b.pop()
	b.push(d.node)
	return true
}
