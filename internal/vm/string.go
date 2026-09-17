package vm

import (
	"math"
	"math/big"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/go-quickjs/go-quickjs/internal/wtf8"
)

// String is a JavaScript string.
//
// Two representation choices matter here.
//
// First, a string is stored as UTF-8 with a flag recording whether it is pure
// ASCII. JavaScript strings are sequences of UTF-16 code units, so indexing and
// length must be in code units; for ASCII, byte offsets and code-unit offsets
// coincide and both are O(1). Only a string containing non-ASCII pays for a
// UTF-16 side table, which is built on first indexed access and cached.
//
// Second, concatenation builds a rope rather than copying. Repeated `s += x` is
// the single most common way a script builds a large string, and copying on
// each step makes that quadratic. A rope defers the copy until something
// actually needs the bytes, at which point the whole tree is flattened once.
type String struct {
	// s is the flattened UTF-8 form. It is valid only when left is nil.
	s string
	// left and right are non-nil for an unflattened rope.
	left, right *String
	// length is the length in UTF-16 code units, and is always valid, rope or
	// not, so that .length never forces a flatten.
	length int
	// ascii reports that every code unit is below 0x80, which makes indexing a
	// byte index. It is always valid.
	ascii bool
	// endsHigh and startsLow record an unpaired surrogate at either end.
	//
	// Joining a high surrogate to a low one makes a code point, and WTF-8
	// spells a code point one way -- so a concatenation across such a boundary
	// has to combine them, or the same string would have two encodings and
	// comparing them by bytes would say they differ. Both are always valid,
	// rope or not, so the check costs nothing on the ordinary path.
	endsHigh, startsLow bool
	// u16 caches the UTF-16 code units of a non-ASCII string.
	u16 []uint16
}

// emptyString is shared, since scripts produce it constantly.
var emptyString = &String{ascii: true}

// NewString returns a String for a Go string.
func NewString(s string) *String {
	if s == "" {
		return emptyString
	}
	ascii, length := scanString(s)
	out := &String{s: s, length: length, ascii: ascii}
	if !ascii {
		out.endsHigh, out.startsLow = wtf8.UnpairedEnds(s)
	}
	return out
}

// scanString reports whether s is pure ASCII and how many UTF-16 code units it
// encodes. The two are computed together because both need one pass.
func scanString(s string) (ascii bool, length int) {
	// The common case is all-ASCII, so check that first with a tight loop.
	i := 0
	for ; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			break
		}
	}
	if i == len(s) {
		return true, len(s)
	}
	// Non-ASCII: count code units, remembering that a rune outside the basic
	// multilingual plane occupies two of them, and that a WTF-8 encoded lone
	// surrogate occupies exactly one despite being three bytes.
	length = i
	for i < len(s) {
		if s[i] < utf8.RuneSelf {
			length++
			i++
			continue
		}
		if _, ok := wtf8.DecodeSurrogateAt(s, i); ok {
			// An encoded lone surrogate is one code unit despite being three
			// bytes.
			length++
			i += 3
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r > 0xFFFF {
			length += 2
		} else {
			length++
		}
		i += size
	}
	return false, length
}

// Len returns the length in UTF-16 code units.
func (s *String) Len() int { return s.length }

// IsEmpty reports whether the string has no code units.
func (s *String) IsEmpty() bool { return s.length == 0 }

// Go returns the string as a Go string, flattening a rope if necessary.
func (s *String) Go() string {
	s.flatten()
	return s.s
}

// flatten collapses a rope into a single contiguous string.
//
// The tree is walked iteratively rather than recursively, because a rope built
// by repeated appends is a left-leaning chain as deep as the number of appends,
// and recursion would overflow the goroutine stack on a long enough loop.
func (s *String) flatten() {
	if s.left == nil {
		return
	}
	var sb strings.Builder
	sb.Grow(s.byteLen())

	// An explicit stack, holding nodes still to be emitted in order.
	stack := make([]*String, 0, 16)
	stack = append(stack, s)
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if n.left == nil {
			sb.WriteString(n.s)
			continue
		}
		// Push right first so that left is emitted first.
		stack = append(stack, n.right, n.left)
	}
	s.s = sb.String()
	s.left, s.right = nil, nil
}

// byteLen returns the number of UTF-8 bytes the string occupies, walking a rope
// without flattening it so that flatten can size its buffer exactly.
func (s *String) byteLen() int {
	if s.left == nil {
		return len(s.s)
	}
	total := 0
	stack := make([]*String, 0, 16)
	stack = append(stack, s)
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if n.left == nil {
			total += len(n.s)
			continue
		}
		stack = append(stack, n.right, n.left)
	}
	return total
}

// Concat returns the concatenation of two strings.
//
// Short results are copied immediately; only a result long enough for the
// deferred copy to pay off becomes a rope. This keeps small concatenations from
// paying for a node they will flatten on the next operation anyway.
func (s *String) Concat(t *String) *String {
	switch {
	case s.length == 0:
		return t
	case t.length == 0:
		return s
	}
	if s.endsHigh && t.startsLow {
		// The join makes a code point out of two halves, which WTF-8 spells as
		// one sequence rather than two. Rare enough to take the slow path.
		return NewString(wtf8.Join(s.Go(), t.Go()))
	}
	ascii := s.ascii && t.ascii
	length := s.length + t.length
	out := &String{length: length, ascii: ascii,
		endsHigh: t.endsHigh, startsLow: s.startsLow}

	const ropeThreshold = 64
	if s.byteLenShallow()+t.byteLenShallow() < ropeThreshold {
		out.s = s.Go() + t.Go()
		return out
	}
	out.left, out.right = s, t
	return out
}

// byteLenShallow returns the byte length without walking a rope, which is all
// Concat needs for its size heuristic.
func (s *String) byteLenShallow() int {
	if s.left == nil {
		return len(s.s)
	}
	// A rope is already at least the threshold, so any large value works.
	return 1 << 20
}

// units returns the UTF-16 code units, building and caching them for a
// non-ASCII string.
func (s *String) units() []uint16 {
	if s.ascii {
		return nil
	}
	if s.u16 == nil {
		s.u16 = wtf8.ToUTF16(s.Go())
	}
	return s.u16
}

// CharCodeAt returns the UTF-16 code unit at i, or -1 if i is out of range.
func (s *String) CharCodeAt(i int) int {
	if i < 0 || i >= s.length {
		return -1
	}
	if s.ascii {
		s.flatten()
		return int(s.s[i])
	}
	return int(s.units()[i])
}

// CodePointAt returns the code point beginning at i, combining a surrogate pair
// when one starts there.
func (s *String) CodePointAt(i int) rune {
	if i < 0 || i >= s.length {
		return -1
	}
	if s.ascii {
		s.flatten()
		return rune(s.s[i])
	}
	u := s.units()
	c := rune(u[i])
	if utf16.IsSurrogate(c) && i+1 < len(u) {
		if r := utf16.DecodeRune(c, rune(u[i+1])); r != utf8.RuneError {
			return r
		}
	}
	return c
}

// Substring returns the code units in [start, end).
func (s *String) Substring(start, end int) *String {
	if start < 0 {
		start = 0
	}
	if end > s.length {
		end = s.length
	}
	if start >= end {
		return emptyString
	}
	if start == 0 && end == s.length {
		return s
	}
	if s.ascii {
		s.flatten()
		return &String{s: s.s[start:end], length: end - start, ascii: true}
	}
	// Slicing UTF-16 can split a surrogate pair, which is legal in JavaScript
	// and produces a lone surrogate. utf16.Decode maps those to U+FFFD, so the
	// pieces are re-encoded unit by unit to preserve them.
	return fromUnits(s.units()[start:end])
}

// fromUnits builds a String from UTF-16 code units.
//
// Slicing between the halves of a surrogate pair is legal in JavaScript and
// yields a lone surrogate, so the encoder must preserve one rather than
// substituting U+FFFD.
func fromUnits(u []uint16) *String {
	s := wtf8.FromUTF16(u)
	return &String{s: s, length: len(u), ascii: wtf8.IsASCII(s)}
}

// Equals reports whether two strings have the same code units.
func (s *String) Equals(t *String) bool {
	if s == t {
		return true
	}
	if t == nil || s.length != t.length {
		return false
	}
	// Both are flattened for the comparison, which is what any subsequent use
	// would force anyway.
	return s.Go() == t.Go()
}

// Compare orders two strings by code unit, as the relational operators require.
func (s *String) Compare(t *String) int {
	if s.ascii && t.ascii {
		// For ASCII, byte order and code-unit order agree.
		return strings.Compare(s.Go(), t.Go())
	}
	a, b := s.forCompare(), t.forCompare()
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

// forCompare returns the code units of a string for ordering purposes.
func (s *String) forCompare() []uint16 {
	if !s.ascii {
		return s.units()
	}
	s.flatten()
	u := make([]uint16, len(s.s))
	for i := 0; i < len(s.s); i++ {
		u[i] = uint16(s.s[i])
	}
	return u
}

// IndexOf returns the code-unit index of the first occurrence of t at or after
// from, or -1.
func (s *String) IndexOf(t *String, from int) int {
	if from < 0 {
		from = 0
	}
	if t.length == 0 {
		if from > s.length {
			return s.length
		}
		return from
	}
	if from >= s.length {
		return -1
	}
	if s.ascii && t.ascii {
		// Byte and code-unit offsets coincide, so strings.Index applies.
		i := strings.Index(s.Go()[from:], t.Go())
		if i < 0 {
			return -1
		}
		return from + i
	}
	a, b := s.units(), t.units()
	if a == nil {
		a = s.forCompare()
	}
	if b == nil {
		b = t.forCompare()
	}
	for i := from; i+len(b) <= len(a); i++ {
		match := true
		for j := range b {
			if a[i+j] != b[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// Symbol
// ---------------------------------------------------------------------------

// Symbol is a JavaScript symbol. Symbols are compared by identity, so the
// struct carries only the description used when printing one.
type Symbol struct {
	Description string
	// HasDescription distinguishes Symbol() from Symbol(undefined), which
	// differ in what String(sym) produces.
	HasDescription bool
	// Registered marks a symbol obtained from Symbol.for, which
	// Symbol.keyFor can look up.
	Registered bool
}

// NewSymbol returns a fresh symbol.
func NewSymbol(desc string, has bool) *Symbol {
	return &Symbol{Description: desc, HasDescription: has}
}

// String renders the symbol as Symbol.prototype.toString does.
func (s *Symbol) String() string {
	return "Symbol(" + s.Description + ")"
}

// ---------------------------------------------------------------------------
// BigInt
// ---------------------------------------------------------------------------

// BigInt is an arbitrary-precision integer.
type BigInt struct {
	V big.Int
}

// NewBigInt returns a BigInt with the given value.
func NewBigInt(v int64) *BigInt {
	b := &BigInt{}
	b.V.SetInt64(v)
	return b
}

// ParseBigInt parses a BigInt literal, which may carry a 0x, 0o or 0b prefix.
//
// The grammar is narrower than Go's own: a digit separator is not allowed here
// -- the lexer removes the ones a literal in source may have -- a sign belongs
// to a decimal literal only, and a leading zero makes nothing octal. So the
// text is checked before big.Int sees it, which otherwise accepts all three.
func ParseBigInt(s string) (*BigInt, bool) {
	digits, base, neg := s, 10, false
	switch {
	case len(digits) > 1 && digits[0] == '0' && (digits[1] == 'x' || digits[1] == 'X'):
		digits, base = digits[2:], 16
	case len(digits) > 1 && digits[0] == '0' && (digits[1] == 'o' || digits[1] == 'O'):
		digits, base = digits[2:], 8
	case len(digits) > 1 && digits[0] == '0' && (digits[1] == 'b' || digits[1] == 'B'):
		digits, base = digits[2:], 2
	case digits != "" && digits[0] == '+':
		digits = digits[1:]
	case digits != "" && digits[0] == '-':
		digits, neg = digits[1:], true
	}
	if digits == "" {
		return nil, false
	}
	for i := 0; i < len(digits); i++ {
		if bigDigitValue(digits[i]) >= base {
			return nil, false
		}
	}
	b := &BigInt{}
	if _, ok := b.V.SetString(digits, base); !ok {
		return nil, false
	}
	if neg {
		b.V.Neg(&b.V)
	}
	return b, true
}

// bigDigitValue is the value of one digit, or a number above every base for a
// character that is not one.
func bigDigitValue(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return 99
}

// StringToBigInt converts a string the way the specification's conversion does:
// the surrounding whitespace is ignored, and a string of nothing but whitespace
// is zero rather than a failure.
func StringToBigInt(s string) (*BigInt, bool) {
	text := strings.Trim(s, jsWhitespace)
	if text == "" {
		return NewBigInt(0), true
	}
	return ParseBigInt(text)
}

// IsZero reports whether the value is zero.
func (b *BigInt) IsZero() bool { return b.V.Sign() == 0 }

// Cmp compares two BigInts.
func (b *BigInt) Cmp(o *BigInt) int { return b.V.Cmp(&o.V) }

// String renders the value in base 10.
func (b *BigInt) String() string { return b.V.String() }

// Float returns the value as a float64, which may lose precision.
func (b *BigInt) Float() float64 {
	f, _ := new(big.Float).SetInt(&b.V).Float64()
	return f
}

// bigIntFromFloat converts an integral float64 to a BigInt exactly, reporting
// false for a value that is not an integer.
//
// The conversion goes through big.Float rather than int64 because a float64 can
// hold integers far beyond the int64 range, and the comparison operators must
// stay exact across that whole range.
func bigIntFromFloat(f float64) (*BigInt, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) {
		return nil, false
	}
	b := &BigInt{}
	if _, acc := new(big.Float).SetFloat64(f).Int(&b.V); acc != big.Exact {
		// SetFloat64 on an integral value is always exact, so this only
		// triggers on a value the guard above should already have rejected.
		return nil, false
	}
	return b, true
}
