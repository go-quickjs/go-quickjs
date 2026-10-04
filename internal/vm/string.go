package vm

import (
	"math"
	"math/big"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jsnum"
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
	// mark is the memory meter's, which it sets on a string it has counted.
	mark uint16
	// u16 caches the UTF-16 code units of a non-ASCII string.
	u16 []uint16
}

// emptyString is shared, since scripts produce it constantly.
var emptyString = &String{ascii: true}

// maxStringLength is the longest a string may be, in code units: V8's,
// 2^29 - 24 -- or on a 32-bit platform 2^28 - 16 -- beyond which making one
// throws "Invalid string length". It keeps every length within an int, and
// a string doubled again and again from growing until writing it out
// exhausts the process, or a 32-bit one's address space.
const maxStringLength = is64Bit*(1<<29-24) + (1-is64Bit)*(1<<28-16)

// is64Bit is 1 where an int has 64 bits, and 0 where it has 32.
const is64Bit = math.MaxInt >> 62 & 1

// throwStringLength refuses a string longer than maxStringLength.
func (r *Runtime) throwStringLength() error {
	return r.throwRangeError("Invalid string length")
}

// builtString is the string a builder made, refused if it is too long.
func (r *Runtime) builtString(s string) (Value, error) {
	out := NewString(s)
	if out.length > maxStringLength {
		return Undefined, r.throwStringLength()
	}
	return Str(out), nil
}

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

// ropeThreshold is the length in bytes from which a concatenation is a rope
// rather than a copy.
const ropeThreshold = 64

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
		// one sequence rather than two. The halves come off the ends and go
		// back as one, leaving the rest of either rope as it is: flattening
		// both made building a string a half at a time quadratic.
		hi, lo := s.lastLeaf().s, t.firstLeaf().s
		pair := NewString(wtf8.Join(hi[len(hi)-3:], lo[:3]))
		return s.withoutLast().Concat(pair).Concat(t.withoutFirst())
	}
	ascii := s.ascii && t.ascii
	length := s.length + t.length

	if n := s.byteLenShallow() + t.byteLenShallow(); n < ropeThreshold {
		out, b := newStringBytes(n)
		out.length, out.ascii, out.endsHigh, out.startsLow = length, ascii, t.endsHigh, s.startsLow
		copy(b[copy(b, s.Go()):], t.Go())
		out.s = unsafe.String(unsafe.SliceData(b), n)
		return out
	}
	out := &String{length: length, ascii: ascii,
		endsHigh: t.endsHigh, startsLow: s.startsLow}
	out.left, out.right = s, t
	return out
}

// newStringBytes makes a String with room for n bytes in the same
// allocation, n at most 72, and returns them to be filled before the String
// is given them: a short string is then one allocation rather than two. Each
// room fills its String up to one of Go's size classes, on a 64-bit platform.
func newStringBytes(n int) (*String, []byte) {
	switch {
	case n <= 8:
		x := new(struct {
			String
			b [8]byte
		})
		return &x.String, x.b[:n]
	case n <= 24:
		x := new(struct {
			String
			b [24]byte
		})
		return &x.String, x.b[:n]
	case n <= 40:
		x := new(struct {
			String
			b [40]byte
		})
		return &x.String, x.b[:n]
	case n <= 56:
		x := new(struct {
			String
			b [56]byte
		})
		return &x.String, x.b[:n]
	}
	x := new(struct {
		String
		b [72]byte
	})
	return &x.String, x.b[:n]
}

// A lone surrogate is three bytes of WTF-8, and the four functions below find
// and take off the one at either end of a string without flattening it.

// lastLeaf is the piece of a rope its last code unit is in.
func (s *String) lastLeaf() *String {
	for s.left != nil {
		s = s.right
	}
	return s
}

// firstLeaf is the piece of a rope its first code unit is in.
func (s *String) firstLeaf() *String {
	for s.left != nil {
		s = s.left
	}
	return s
}

// withoutLast is s without the lone high surrogate it ends with.
func (s *String) withoutLast() *String {
	var spine []*String
	for n := s; n.left != nil; n = n.right {
		spine = append(spine, n)
	}
	leaf := s.lastLeaf().s
	out := NewString(leaf[:len(leaf)-3])
	for i := len(spine) - 1; i >= 0; i-- {
		out = spine[i].left.Concat(out)
	}
	return out
}

// withoutFirst is s without the lone low surrogate it starts with.
func (s *String) withoutFirst() *String {
	var spine []*String
	for n := s; n.left != nil; n = n.left {
		spine = append(spine, n)
	}
	out := NewString(s.firstLeaf().s[3:])
	for i := len(spine) - 1; i >= 0; i-- {
		out = out.Concat(spine[i].right)
	}
	return out
}

// prefix is at most the first max bytes of s, cut where a character begins,
// read off a rope without flattening it.
func (s *String) prefix(max int) string {
	if s.left == nil && len(s.s) <= max {
		return s.s
	}
	var sb strings.Builder
	stack := []*String{s}
	for len(stack) > 0 && sb.Len() < max {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if n.left == nil {
			sb.WriteString(n.s[:min(len(n.s), max-sb.Len())])
			continue
		}
		stack = append(stack, n.right, n.left)
	}
	// A character cut short is left out.
	out := sb.String()
	i := len(out) - 1
	for i > 0 && out[i]&0xC0 == 0x80 {
		i--
	}
	if i >= 0 && i+utf8SeqLen(out[i]) > len(out) {
		out = out[:i]
	}
	return out
}

// utf8SeqLen is how long a UTF-8 sequence is that begins with b.
func utf8SeqLen(b byte) int {
	switch {
	case b < 0xC0:
		return 1
	case b < 0xE0:
		return 2
	case b < 0xF0:
		return 3
	}
	return 4
}

// partsBuilder assembles a string out of pieces a script can see, joining the
// halves of a surrogate pair that meet at a boundary.
//
// A string is its code units, so two halves that meet are the character the
// pair spells -- and a character has one WTF-8 spelling, without which two
// strings with the same code units would not compare equal. The last piece is
// held back rather than written, so that the one after it can still be joined
// to it; everything before is already settled.
type partsBuilder struct {
	sb   strings.Builder
	last string
	// endsHigh records that the piece held back ends with an unpaired high
	// surrogate, which is the only thing the next piece could complete.
	endsHigh bool
	// counting records that what has been written is long enough for its
	// code units to be counted, and units is how many sb holds.
	counting bool
	units    int
}

// Grow reserves room for n more bytes.
func (b *partsBuilder) Grow(n int) { b.sb.Grow(n) }

// overlong reports whether what has been written is already too long to be
// a string, which a builder asks as it goes so that it gives up before it has
// built what could never be returned. A code unit takes at least a byte, so
// nothing is counted until there are more bytes than a string may have code
// units; from then on the code units are counted as they are written.
func (b *partsBuilder) overlong() bool {
	if b.sb.Len()+len(b.last) <= maxStringLength {
		return false
	}
	if !b.counting {
		b.counting = true
		_, b.units = scanString(b.sb.String())
	}
	_, last := scanString(b.last)
	return b.units+last > maxStringLength
}

// flush writes the piece held back.
func (b *partsBuilder) flush() {
	b.sb.WriteString(b.last)
	if b.counting {
		_, n := scanString(b.last)
		b.units += n
	}
}

// WriteString adds a piece.
func (b *partsBuilder) WriteString(s string) {
	if s == "" {
		return
	}
	endsHigh, startsLow := wtf8.UnpairedEnds(s)
	b.write(s, endsHigh, startsLow)
}

// WriteStr adds a String, whose unpaired ends it knows already.
func (b *partsBuilder) WriteStr(s *String) {
	if s.length != 0 {
		b.write(s.Go(), s.endsHigh, s.startsLow)
	}
}

// write adds a piece that is not empty, with its unpaired ends.
func (b *partsBuilder) write(s string, endsHigh, startsLow bool) {
	if b.endsHigh && startsLow {
		b.last = wtf8.Join(b.last, s)
		b.endsHigh, _ = wtf8.UnpairedEnds(b.last)
		return
	}
	b.flush()
	b.last, b.endsHigh = s, endsHigh
}

// String returns what has been written.
func (b *partsBuilder) String() string {
	b.flush()
	b.last, b.endsHigh = "", false
	return b.sb.String()
}

// joinValues builds the string a template literal produces.
//
// The pieces go into one buffer rather than being concatenated in turn: a
// template with k substitutions would otherwise build k intermediate strings,
// and a number among them would cost a string of its own before being copied
// into the next. Every part must already be a string or a number -- the caller
// converts anything else first, because a toString may run user code and when
// it does is fixed.
func joinValues(parts []Value) *String {
	if len(parts) == 0 {
		return emptyString
	}

	// A join that pairs a trailing high surrogate with a leading low one makes
	// one code point out of two halves, which WTF-8 spells as a single
	// sequence. It is rare, and Concat already knows how.
	size, prevHigh := 0, false
	for _, p := range parts {
		if !p.IsString() {
			// A number's text is short and ASCII, so it can neither pair with
			// a surrogate nor be worth measuring exactly.
			size += 24
			prevHigh = false
			continue
		}
		s := p.String()
		if prevHigh && s.startsLow {
			return concatPairwise(parts)
		}
		size += s.byteLenShallow()
		prevHigh = s.endsHigh
	}

	var sb strings.Builder
	sb.Grow(size)
	units, ascii := 0, true
	var tmp [32]byte
	for _, p := range parts {
		if !p.IsString() {
			b := jsnum.AppendFloat(tmp[:0], p.Number())
			sb.Write(b)
			units += len(b)
			continue
		}
		s := p.String()
		sb.WriteString(s.Go())
		units += s.length
		ascii = ascii && s.ascii
	}
	if units == 0 {
		return emptyString
	}
	out := &String{s: sb.String(), length: units, ascii: ascii}
	if first := parts[0]; first.IsString() {
		out.startsLow = first.String().startsLow
	}
	if last := parts[len(parts)-1]; last.IsString() {
		out.endsHigh = last.String().endsHigh
	}
	return out
}

// concatPairwise joins the parts one at a time, which is what a join across a
// surrogate pair needs.
func concatPairwise(parts []Value) *String {
	out := emptyString
	for i, p := range parts {
		s := p.String()
		if !p.IsString() {
			s = NewString(jsnum.FormatFloat(p.Number()))
		}
		if i == 0 {
			out = s
			continue
		}
		out = out.Concat(s)
	}
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

// codeUnits returns the UTF-16 code units of the string, caching them.
//
// It differs from units in answering for an ASCII string too: indexing one does
// not need the units, but matching a pattern against it does, and matching the
// same string over and over is what a program does with a pattern. The slice is
// the string's own and must not be written to.
func (s *String) codeUnits() []uint16 {
	if s.length == 0 {
		// Nothing to cache -- and the empty string is shared by every
		// runtime, which a write to it would race between.
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

// unitSlice is the WTF-8 text of code units [i, j) of s, whose code units are
// units. Text that is all ASCII is its own code units, and is cut rather than
// encoded again.
func (s *String) unitSlice(units []uint16, i, j int) string {
	if s.ascii {
		return s.Go()[i:j]
	}
	return wtf8.FromUTF16(units[i:j])
}

// fromUnits builds a String from UTF-16 code units.
//
// Slicing between the halves of a surrogate pair is legal in JavaScript and
// yields a lone surrogate, so the encoder must preserve one rather than
// substituting U+FFFD.
func fromUnits(u []uint16) *String {
	s, ascii := wtf8.FromUTF16ASCII(u)
	out := &String{s: s, length: len(u), ascii: ascii}
	if !out.ascii {
		// Which halves are unpaired is what a concatenation looks at, so a
		// string built here has to know as much about itself as one built by
		// NewString does.
		out.endsHigh, out.startsLow = wtf8.UnpairedEnds(s)
	}
	return out
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
	return indexUnits(s.unitsOrCompare(), t.unitsOrCompare(), from)
}

// LastIndexOf returns the last code-unit index at or before end where t
// occurs in s, or -1.
func (s *String) LastIndexOf(t *String, end int) int {
	end = min(end, s.length-t.length)
	if end < 0 {
		return -1
	}
	if s.ascii && t.ascii {
		return strings.LastIndex(s.Go()[:end+t.length], t.Go())
	}
	return lastIndexUnits(s.unitsOrCompare(), t.unitsOrCompare(), end)
}

// unitsOrCompare is the string's code units, however it holds them.
func (s *String) unitsOrCompare() []uint16 {
	if u := s.units(); u != nil {
		return u
	}
	return s.forCompare()
}

// shortNeedle is the length up to which a search compares at each position:
// linear in practice for a needle this short, and cheaper than a table.
const shortNeedle = 8

// indexUnits finds b in a from position from, in time linear in both: a
// needle past shortNeedle long is searched for by Knuth, Morris and Pratt's
// method, so that one that almost matches everywhere is not compared again
// at every position.
func indexUnits(a, b []uint16, from int) int {
	if len(b) <= shortNeedle {
		for i := from; i+len(b) <= len(a); i++ {
			j := 0
			for j < len(b) && a[i+j] == b[j] {
				j++
			}
			if j == len(b) {
				return i
			}
		}
		return -1
	}
	fail := kmpTable(b, false)
	j := 0
	for i := from; i < len(a); i++ {
		for j > 0 && a[i] != b[j] {
			j = fail[j-1]
		}
		if a[i] == b[j] {
			j++
			if j == len(b) {
				return i - len(b) + 1
			}
		}
	}
	return -1
}

// lastIndexUnits is indexUnits searching backwards from end, the last
// position a match may start at.
func lastIndexUnits(a, b []uint16, end int) int {
	if len(b) <= shortNeedle {
		for i := end; i >= 0; i-- {
			j := 0
			for j < len(b) && a[i+j] == b[j] {
				j++
			}
			if j == len(b) {
				return i
			}
		}
		return -1
	}
	// The needle is matched from its end, against the text read backwards
	// from where a match starting at end would end.
	fail := kmpTable(b, true)
	m, j := len(b), 0
	for i := end + m - 1; i >= 0; i-- {
		for j > 0 && a[i] != b[m-1-j] {
			j = fail[j-1]
		}
		if a[i] == b[m-1-j] {
			j++
			if j == m {
				return i
			}
		}
	}
	return -1
}

// kmpTable is the failure function of b, or of b reversed.
func kmpTable(b []uint16, reversed bool) []int {
	at := func(i int) uint16 {
		if reversed {
			return b[len(b)-1-i]
		}
		return b[i]
	}
	fail := make([]int, len(b))
	k := 0
	for i := 1; i < len(b); i++ {
		for k > 0 && at(i) != at(k) {
			k = fail[k-1]
		}
		if at(i) == at(k) {
			k++
		}
		fail[i] = k
	}
	return fail
}

// ---------------------------------------------------------------------------
// Symbol
// ---------------------------------------------------------------------------

// Symbol is a JavaScript symbol. Symbols are compared by identity, so the
// struct carries only what printing one needs, and what being a WeakMap key
// does.
type Symbol struct {
	Description string
	// weakMapRefs holds the symbol's values in the WeakMaps it is a key of,
	// made when it first becomes one (see weakmap.go). Only an unregistered
	// symbol can be.
	weakMapRefs *weakMapRefs
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
//
// A BigInt is never copied: one newBigResult made keeps its digits in the
// allocation that holds it, which a copy's V would go on pointing into.
type BigInt struct {
	V big.Int
}

// NewBigInt returns a BigInt with the given value.
func NewBigInt(v int64) *BigInt {
	b := newBigResult()
	b.V.SetInt64(v)
	return b
}

// smallBigInt is a BigInt and room for four words of digits: one allocation
// where a BigInt and its digits are two. A BigInt in the int64 range is held
// in its Value, so one on the heap has two words or more, and big.Int asks
// for a word more than a sum's longer operand has before it knows whether
// the sum needs it. Three words would take the same 64-byte size class.
type smallBigInt struct {
	b     BigInt
	words [4]big.Word
}

// smallBigWords is how many words of digits a smallBigInt has room for.
const smallBigWords = len(smallBigInt{}.words)

// newBigResult is a BigInt of zero that keeps its digits in its own
// allocation while they fit in smallBigWords, for a result that will.
func newBigResult() *BigInt {
	s := &smallBigInt{}
	s.b.V.SetBits(s.words[:0])
	return &s.b
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
	if f > -(1<<63) && f < 1<<63 {
		// What an int64 holds converts exactly, and most are this small.
		return NewBigInt(int64(f)), true
	}
	b := &BigInt{}
	if _, acc := new(big.Float).SetFloat64(f).Int(&b.V); acc != big.Exact {
		// SetFloat64 on an integral value is always exact, so this only
		// triggers on a value the guard above should already have rejected.
		return nil, false
	}
	return b, true
}
