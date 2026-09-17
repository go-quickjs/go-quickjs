package vm

import (
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/wtf8"
)

// Recent additions to the standard: Promise.try, Error.isError, RegExp.escape
// and Math.sumPrecise.
//
// They have nothing in common beyond being small and recent, so they live
// together rather than each adding a file.

func (r *Runtime) initRecentBuiltins() {
	r.initPromiseTry()
	r.initErrorIsError()
	r.initRegExpEscape()
	r.initMathSumPrecise()
}

// Promise.try runs a function immediately and wraps whatever happens in a
// promise.
//
// It exists because `Promise.resolve().then(fn)` defers fn to a later turn,
// while `new Promise(res => res(fn()))` runs it now but is easy to get subtly
// wrong. Promise.try runs fn synchronously and turns a synchronous throw into a
// rejection, which is what callers writing the second form actually wanted.
func (r *Runtime) initPromiseTry() {
	ctor, ok := r.globalFunc("Promise")
	if !ok {
		return
	}
	r.defMethod(ctor, "try", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		// The receiver is the constructor, so a subclass's Promise.try yields
		// an instance of the subclass -- and a promise the callback returns is
		// adopted rather than wrapped in another.
		cap, err := rt.newPromiseCapability(this)
		if err != nil {
			return Undefined, err
		}
		fn := arg(args, 0)
		var rest []Value
		if len(args) > 1 {
			rest = args[1:]
		}
		v, callErr := rt.call(fn, Undefined, rest)
		if callErr != nil {
			if _, err := rt.call(cap.reject, Undefined, []Value{thrownValue(callErr)}); err != nil {
				return Undefined, err
			}
			return Obj(cap.promise), nil
		}
		if _, err := rt.call(cap.resolve, Undefined, []Value{v}); err != nil {
			return Undefined, err
		}
		return Obj(cap.promise), nil
	})
}

// Error.isError reports whether a value is a genuine Error.
//
// `instanceof Error` cannot answer this: it follows a prototype chain a script
// can forge, and it gives the wrong answer for an error from another realm.
// isError reads the internal slot instead, so only an object the Error
// machinery created says yes.
func (r *Runtime) initErrorIsError() {
	ctor, ok := r.globalFunc("Error")
	if !ok {
		return
	}
	r.defMethod(ctor, "isError", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		v := arg(args, 0)
		return Bool(v.IsObject() && v.Object().class == ClassError), nil
	})
}

// RegExp.escape makes a string safe to splice into a pattern.
//
// Hand-rolled escaping is a recurring source of injection bugs, and the obvious
// character class misses the cases that matter. This follows the specification:
// a leading identifier character is escaped numerically so that the result can
// never merge with whatever precedes it, punctuation that could be confused
// with syntax is escaped even where it is harmless, and code units outside the
// printable range are spelled out.
func (r *Runtime) initRegExpEscape() {
	ctor, ok := r.globalFunc("RegExp")
	if !ok {
		return
	}
	r.defMethod(ctor, "escape", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		v := arg(args, 0)
		if !v.IsString() {
			// Deliberately not ToString: escaping a number or an object would
			// silently produce a pattern the caller did not write.
			return Undefined, rt.throwTypeError("RegExp.escape requires a string")
		}
		return Str(NewString(escapeRegExp(v.String().Go()))), nil
	})
}

// syntaxChars are the characters that mean something in a pattern.
const syntaxChars = `^$\.*+?()[]{}|/`

// otherPunctuators are escaped numerically even though they are not syntax, so
// that the result is safe to splice into a class or a group name as well as
// into a pattern.
const otherPunctuators = ",-=<>#&!%:;@~'`\"" + " "

func escapeRegExp(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	units := wtf8.ToUTF16(s)
	for i, u := range units {
		// A leading identifier character is escaped numerically, so that the
		// escaped string cannot combine with a preceding \ or a quantifier.
		if i == 0 && isIdentifierCodeUnit(u) {
			b.WriteString(hexEscape(u))
			continue
		}
		switch {
		case u < 128 && strings.ContainsRune(syntaxChars, rune(u)):
			b.WriteByte('\\')
			b.WriteRune(rune(u))
		case u < 128 && strings.ContainsRune(otherPunctuators, rune(u)):
			b.WriteString(hexEscape(u))
		default:
			switch u {
			case '\t':
				b.WriteString(`\t`)
			case '\n':
				b.WriteString(`\n`)
			case '\v':
				b.WriteString(`\v`)
			case '\f':
				b.WriteString(`\f`)
			case '\r':
				b.WriteString(`\r`)
			case 0x00A0, 0x2028, 0x2029, 0xFEFF:
				// Whitespace that is invisible in source, and so worth
				// spelling out.
				b.WriteString(hexEscape(u))
			default:
				if u < 0x20 || (u >= 0xD800 && u <= 0xDFFF) {
					b.WriteString(hexEscape(u))
				} else {
					b.Write(wtf8.AppendRune(nil, rune(u)))
				}
			}
		}
	}
	return b.String()
}

func isIdentifierCodeUnit(u uint16) bool {
	return u == '_' || u == '$' ||
		(u >= '0' && u <= '9') ||
		(u >= 'a' && u <= 'z') ||
		(u >= 'A' && u <= 'Z')
}

// hexEscape spells a code unit as \xHH when it fits in a byte and \uHHHH
// otherwise.
func hexEscape(u uint16) string {
	if u <= 0xFF {
		return `\x` + pad(strconv.FormatUint(uint64(u), 16), 2)
	}
	return `\u` + pad(strconv.FormatUint(uint64(u), 16), 4)
}

func pad(s string, n int) string {
	for len(s) < n {
		s = "0" + s
	}
	return s
}

// Math.sumPrecise adds a list of numbers with a single rounding at the end.
//
// Summing float64s in a loop rounds at every step, so the result depends on the
// order and can be arbitrarily far from the true total: [1e20, 0.1, -1e20]
// gives 0 naively and 0.1 here. Doing the arithmetic in arbitrary precision and
// rounding once is the straightforward way to get the exact answer, and this
// is not a hot path.
func (r *Runtime) initMathSumPrecise() {
	m, ok := r.globalObj("Math")
	if !ok {
		return
	}
	r.defMethod(m, "sumPrecise", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		cursor, err := rt.startForOf(arg(args, 0))
		if err != nil {
			return Undefined, err
		}

		// Infinities and NaN cannot participate in the exact sum, so they are
		// tracked separately: a NaN anywhere wins, and infinities of both
		// signs make the result NaN.
		sum := new(big.Float).SetPrec(2048)
		sawNaN := false
		posInf, negInf := false, false
		count := 0

		for {
			v, ok, err := rt.iterNext(cursor)
			if err != nil {
				return Undefined, err
			}
			if !ok {
				break
			}
			if !v.IsNumber() {
				rt.closeIter(cursor)
				return Undefined, rt.throwTypeError("Math.sumPrecise requires numbers")
			}
			n := v.Number()
			count++
			switch {
			case math.IsNaN(n):
				sawNaN = true
			case math.IsInf(n, 1):
				posInf = true
			case math.IsInf(n, -1):
				negInf = true
			default:
				sum.Add(sum, new(big.Float).SetPrec(2048).SetFloat64(n))
			}
		}

		switch {
		case sawNaN, posInf && negInf:
			return Float(math.NaN()), nil
		case posInf:
			return Float(math.Inf(1)), nil
		case negInf:
			return Float(math.Inf(-1)), nil
		case count == 0:
			// The sum of nothing is -0, so that it is the identity for a
			// following addition of -0.
			return Float(math.Copysign(0, -1)), nil
		}
		f, _ := sum.Float64()
		if f == 0 {
			// An exact zero from cancelling values is +0, not the -0 the empty
			// case produces.
			return Float(0), nil
		}
		return Float(f), nil
	})
}

// globalFunc looks up a global that should be a constructor.
func (r *Runtime) globalFunc(name string) (*Object, bool) { return r.globalObj(name) }

func (r *Runtime) globalObj(name string) (*Object, bool) {
	p := r.global.getOwn(r.atoms.intern(name))
	if p == nil || !p.value.IsObject() {
		return nil, false
	}
	return p.value.Object(), true
}
