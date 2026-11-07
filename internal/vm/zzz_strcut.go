package vm

import (
	"math/bits"
	"strings"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// A string cut from another under a memory limit (KI-77).
//
// A cut shares its parent's bytes, and so keeps all of them alive, but the
// walk counts it at its own length: nothing says what it holds. So in a
// runtime with a limit a cut shares bytes only if it is at least half the size
// of its root, the string whose bytes they are, which bounds what a string
// holds to twice what it is counted at. A shorter cut is copied: it then
// counts exactly, and is a root itself. A runtime with no limit is not asked
// and cuts as it always has; kept is a nil check, inlined, past which it runs
// no code of this file.
//
// A chain of halvings, s.slice(n/2).slice(n/4)..., must not creep past the
// rule by being half of its parent each time, so a shared cut records its
// root's size, rounded up to a power of two, as a log2 in the byte ropes also
// is. The rounding is up, so that half of it is at least half of the root.
// The byte is free on these strings: strAppend reads it only on a string with
// building set, which is a rope it made itself, and a cut is flat and is never
// building; and it writes it only on such a rope. No other code and no native
// code reads it (the JIT's offsets are ropes-free: zzjit_ssa.go's String
// fields). strRooted's bit keeps strAppend's small counts, which a string from
// a runtime without a limit may still have, from being taken for a size.
//
// s = s.slice(1) in a loop stays linear: it shares until it is below half,
// copies once, and is a root of half the size that shares again. Rounding
// costs at most one copy earlier than half of an exact size would have.

// strRooted marks a value of String.ropes that says a cut's root's size.
const strRooted = 0x80

// kept is v, cut by a built-in from s, as the script is to have it: where s's
// runtime has a limit, v goes by the rule above. What a built-in returns or
// hands a callback, and could be kept, goes through here.
func (r *Runtime) kept(s, v *String) *String {
	if r.meter != nil {
		return keptCut[struct{}](s, v)
	}
	return v
}

// ownText is text that may be a slice of something larger -- a script's source,
// a document -- for a value or an object to keep: a copy, where there is a
// limit that would not see what the slice holds.
func (r *Runtime) ownText(text string) string {
	if r.meter != nil {
		return strings.Clone(text)
	}
	return text
}

// keptCut applies the rule to v, a string just made, which it may empty: a
// cut is made a String of its own, past whatever room it was cut into. A String
// in a room keeps the allocation it is in alive -- a match array with its
// input, or all of a match's groups -- which nothing counts, and the room, left
// holding the bytes, would keep them. A v with bytes of its own stays as it is.
// Generic, as asciiParts is, to be laid out after every closure.
func keptCut[_ any](s, v *String) *String {
	n := len(v.s)
	if v == s || n == 0 || !strWithin(v.s, s.s) {
		return v
	}
	var out *String
	root, mark := strRootOf(s)
	// As 2n >= root, which can overflow an int on a 32-bit platform.
	if int64(n) >= root-root/2 {
		out = new(String)
		*out = *v
		out.ropes = mark
	} else {
		var b []byte
		out, b = newFlatBytes(n)
		copy(b, v.s)
		out.setBytes(b)
		out.length, out.ascii, out.endsHigh, out.startsLow = v.length, v.ascii, v.endsHigh, v.startsLow
	}
	*v = String{}
	return out
}

// What follows is the part of the built-ins' closures that Go lays out
// between the tree tier's closures and its appends' generics (strAppend):
// what is added to those closures moves the one from the other, which no
// placement varies, and string_build1's r += "x" ran 16% slower for 512 bytes
// of it. So such a closure calls one of these, out of line, where it called
// what they call, and keeps its size.

// parseJSONOf is JSON.parse's parse of s, which is its document under a limit.
//
//go:noinline
func parseJSONOf[_ any](r *Runtime, s *String, records bool) (Value, *jsonRecord, error) {
	var doc *String
	if r.meter != nil {
		doc = s
	}
	return r.parseJSON(s.Go(), records, doc)
}

// ownString is NewString of text that may be a slice of something larger.
//
//go:noinline
func ownString[_ any](r *Runtime, text string) *String {
	return NewString(r.ownText(text))
}

// ownGo is s's text, which under a limit is copied: a Symbol's description,
// and the key of Symbol.for's registry, which would come back from
// Symbol.prototype.description and Symbol.keyFor with no mark of a cut.
//
//go:noinline
func ownGo[_ any](r *Runtime, s *String) string {
	return r.ownText(s.Go())
}

// jsonKept is a string value of the document, a slice of it unless it has an
// escape, under a limit: it goes by the rule, the document its parent. It has
// an allocation of its own, unlike a string of the batches of a document
// (keepText, newString), one of which a survivor would keep alive at no count.
func jsonKept[_ any](p *jsonParser, s string) Value {
	return Str(keptCut[struct{}](p.doc, NewString(s)))
}

// ownEvaluator is fn for a runtime with a limit. The lexer's names and strings
// are slices of the source, the script's string, which outlive the code by
// what the code makes of them: a constant's value, a name's atom, for as long
// as the runtime. So each is given bytes of its own. The code itself and its
// text are the code's, and go with it.
func ownEvaluator[_ any](fn Evaluator) Evaluator {
	return func(source string, req EvalRequest) (*bytecode.Function, error) {
		f, err := fn(source, req)
		if err == nil {
			ownCode[struct{}](f)
		}
		return f, err
	}
}

// ownCode copies what f and the functions in it keep of their source, past
// their own lives, out of it. It has just been compiled, and nothing shares it.
func ownCode[_ any](f *bytecode.Function) {
	f.Name = strings.Clone(f.Name)
	f.TraceName = strings.Clone(f.TraceName)
	own := func(names []string) {
		for i, n := range names {
			names[i] = strings.Clone(n)
		}
	}
	own(f.Names)
	for i := range f.Constants {
		c := &f.Constants[i]
		c.Str = strings.Clone(c.Str)
		// An empty slice of the source holds it as well as a long one does.
		c.Flags = strings.Clone(c.Flags)
		if c.Fn != nil {
			ownCode[struct{}](c.Fn)
		}
	}
	for i := range f.Templates {
		own(f.Templates[i].Cooked)
		own(f.Templates[i].Raw)
	}
}

// strRootOf is the size in bytes of the root of a flat s, rounded up to a
// power of two if s is a cut, and what a cut of it records. A string with no
// mark is its own root. An int64: a 32-bit platform's largest root rounds up
// to 1<<31.
func strRootOf(s *String) (root int64, mark uint8) {
	if s.ropes&strRooted != 0 {
		return 1 << (s.ropes &^ strRooted), s.ropes
	}
	return int64(len(s.s)), strRooted | uint8(bits.Len64(uint64(len(s.s))-1))
}

// strWithin reports whether the bytes of t, not empty, are among those of s.
func strWithin(t, s string) bool {
	from := uintptr(unsafe.Pointer(unsafe.StringData(s)))
	at := uintptr(unsafe.Pointer(unsafe.StringData(t)))
	return at >= from && at-from < uintptr(len(s))
}
