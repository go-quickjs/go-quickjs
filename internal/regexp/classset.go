package regexp

import (
	"sort"
	"unicode/utf16"
	"unicode/utf8"
)

// Class sets: the character-class syntax the `v` flag enables.
//
// Under `u` a class is a flat list of characters and ranges, and combining two
// of them means writing both out. Under `v` a class is a set expression:
// classes nest, `--` subtracts, `&&` intersects, and `\q{...}` names whole
// strings. So [\p{Letter}--[aeiou]] says what it means instead of enumerating
// the difference by hand.
//
// A class that can match a string rather than a single code point is the part
// that does not fit the existing matcher, which tests one character at a time.
// Such a class compiles to an alternation -- the strings, longest first, then
// the character set -- which is exactly what it denotes and needs no new
// instruction.

// classSetValue is what a class expression denotes.
type classSetValue struct {
	set *charSet
	// strings holds the multi-character sequences \q{} contributed. A
	// single-character sequence is a code point and joins set instead, which is
	// what makes [\q{a}] and [a] the same class.
	strings [][]rune
}

func newClassSetValue() *classSetValue {
	return &classSetValue{set: newCharSet()}
}

// addString records a sequence, folding the one- and zero-character cases into
// the forms that already exist.
func (v *classSetValue) addString(s []rune) {
	if len(s) == 1 {
		v.set.addChar(s[0])
		return
	}
	for _, have := range v.strings {
		if sameRunes(have, s) {
			return
		}
	}
	v.strings = append(v.strings, s)
}

func sameRunes(a, b []rune) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// union adds everything in other.
func (v *classSetValue) union(other *classSetValue) {
	v.set.addSet(other.set)
	for _, s := range other.strings {
		v.addString(s)
	}
}

// intersect keeps only what appears in both.
func (v *classSetValue) intersect(other *classSetValue) {
	v.set = setFromRanges(intersectRanges(v.set.positive(), other.set.positive()),
		v.set.foldCase)
	var kept [][]rune
	for _, s := range v.strings {
		for _, t := range other.strings {
			if sameRunes(s, t) {
				kept = append(kept, s)
				break
			}
		}
	}
	v.strings = kept
}

// subtract removes everything in other.
func (v *classSetValue) subtract(other *classSetValue) {
	v.set = setFromRanges(subtractRanges(v.set.positive(), other.set.positive()),
		v.set.foldCase)
	var kept [][]rune
	for _, s := range v.strings {
		found := false
		for _, t := range other.strings {
			if sameRunes(s, t) {
				found = true
				break
			}
		}
		if !found {
			kept = append(kept, s)
		}
	}
	v.strings = kept
}

// complement inverts the character set.
//
// A class that can match a string has no complement -- there is no sensible
// "every string but these" -- which is why [^\q{ab}] is a syntax error rather
// than an empty class.
func (v *classSetValue) complement() {
	v.set = setFromRanges(complementRanges(v.set.positive()), v.set.foldCase)
}

// node renders the value as something the compiler can emit.
func (v *classSetValue) node() node {
	// The matcher's membership test assumes sorted, coalesced ranges, which
	// the union path does not otherwise produce.
	v.set.normalize()
	if len(v.strings) == 0 {
		return nodeClass{set: v.set}
	}
	// Longest first: a class holding both "abc" and "a" must prefer "abc",
	// since the alternation commits to the first branch that matches.
	strs := append([][]rune(nil), v.strings...)
	sort.SliceStable(strs, func(i, j int) bool { return len(strs[i]) > len(strs[j]) })

	alts := make([]node, 0, len(strs)+1)
	for _, s := range strs {
		items := make([]node, len(s))
		for i, c := range s {
			items[i] = nodeChar{r: c}
		}
		alts = append(alts, nodeSeq{items: items})
	}
	if len(v.set.ranges) > 0 {
		alts = append(alts, nodeClass{set: v.set})
	}
	if len(alts) == 1 {
		return alts[0]
	}
	return nodeAlt{alts: alts}
}

// positive returns a set's ranges with negation resolved, so that the set
// operations have something concrete to work on.
func (s *charSet) positive() []charRange {
	rs := normalizedRanges(s.ranges)
	if s.negated {
		return complementRanges(rs)
	}
	return rs
}

// setFromRanges rebuilds a set from a range list.
func setFromRanges(rs []charRange, fold bool) *charSet {
	return &charSet{ranges: rs, foldCase: fold}
}

// normalizedRanges sorts and merges a range list, which every operation below
// assumes of its inputs.
func normalizedRanges(in []charRange) []charRange {
	if len(in) == 0 {
		return nil
	}
	rs := append([]charRange(nil), in...)
	sort.Slice(rs, func(i, j int) bool { return rs[i].lo < rs[j].lo })
	out := rs[:1]
	for _, r := range rs[1:] {
		last := &out[len(out)-1]
		if r.lo <= last.hi+1 {
			if r.hi > last.hi {
				last.hi = r.hi
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// intersectRanges returns what both lists cover.
func intersectRanges(a, b []charRange) []charRange {
	var out []charRange
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		lo, hi := a[i].lo, a[i].hi
		if b[j].lo > lo {
			lo = b[j].lo
		}
		if b[j].hi < hi {
			hi = b[j].hi
		}
		if lo <= hi {
			out = append(out, charRange{lo, hi})
		}
		if a[i].hi < b[j].hi {
			i++
		} else {
			j++
		}
	}
	return out
}

// subtractRanges returns what a covers and b does not.
func subtractRanges(a, b []charRange) []charRange {
	return intersectRanges(a, complementRanges(b))
}

// parseClassSet parses a `v`-mode character class.
//
// The body is one of three shapes, decided by the first operator it contains:
// a union (the default, written by juxtaposition), an intersection chained with
// `&&`, or a difference chained with `--`. Mixing them without brackets is a
// syntax error, because `a--b&&c` has no agreed precedence and guessing one
// would silently mean something.
func (p *parser) parseClassSet() (node, error) {
	p.pos++ // consume '['
	negated := p.eat('^')

	v, err := p.parseClassSetBody()
	if err != nil {
		return nil, err
	}
	if !p.eat(']') {
		return nil, p.errorf("unterminated character class")
	}
	if negated {
		if len(v.strings) > 0 {
			// There is no sensible "every string but these".
			return nil, p.errorf("a negated class cannot contain a string")
		}
		v.complement()
	}
	return v.node(), nil
}

// parseClassSetBody parses the contents of a class up to its closing bracket.
func (p *parser) parseClassSetBody() (*classSetValue, error) {
	acc, err := p.parseClassSetOperand()
	if err != nil {
		return nil, err
	}
	if acc == nil {
		// An empty class matches nothing, which is what [] already means.
		return newClassSetValue(), nil
	}

	// The first operator fixes which one the rest of the body may use.
	switch {
	case p.peek() == '&' && p.peekAt(1) == '&':
		for p.peek() == '&' && p.peekAt(1) == '&' {
			p.pos += 2
			if p.peek() == '&' {
				return nil, p.errorf("\"&&&\" is not a valid class operator")
			}
			rhs, err := p.parseClassSetOperand()
			if err != nil {
				return nil, err
			}
			if rhs == nil {
				return nil, p.errorf("\"&&\" needs an operand on both sides")
			}
			acc.intersect(rhs)
		}
	case p.peek() == '-' && p.peekAt(1) == '-':
		for p.peek() == '-' && p.peekAt(1) == '-' {
			p.pos += 2
			rhs, err := p.parseClassSetOperand()
			if err != nil {
				return nil, err
			}
			if rhs == nil {
				return nil, p.errorf("\"--\" needs an operand on both sides")
			}
			acc.subtract(rhs)
		}
	default:
		for {
			rhs, err := p.parseClassSetOperand()
			if err != nil {
				return nil, err
			}
			if rhs == nil {
				break
			}
			acc.union(rhs)
		}
	}
	if p.peek() == '&' && p.peekAt(1) == '&' || p.peek() == '-' && p.peekAt(1) == '-' {
		return nil, p.errorf("a class cannot mix set operators without brackets")
	}
	return acc, nil
}

// parseClassSetOperand parses one operand, returning nil at the end of the
// class or before an operator.
func (p *parser) parseClassSetOperand() (*classSetValue, error) {
	if p.atEnd() {
		return nil, p.errorf("unterminated character class")
	}
	switch {
	case p.peek() == ']':
		return nil, nil
	case p.peek() == '&' && p.peekAt(1) == '&':
		return nil, nil
	case p.peek() == '-' && p.peekAt(1) == '-':
		return nil, nil
	case p.peek() == '[':
		// A nested class, which is what makes the operators worth having.
		v := newClassSetValue()
		p.pos++
		inner := p.eat('^')
		body, err := p.parseClassSetBody()
		if err != nil {
			return nil, err
		}
		if !p.eat(']') {
			return nil, p.errorf("unterminated character class")
		}
		if inner {
			if len(body.strings) > 0 {
				return nil, p.errorf("a negated class cannot contain a string")
			}
			body.complement()
		}
		v.union(body)
		return v, nil
	}

	return p.parseClassSetChars()
}

// parseClassSetChars parses a single character, a shorthand or a \q{} string
// list, plus the range that may follow a single character.
func (p *parser) parseClassSetChars() (*classSetValue, error) {
	lo, kind, set, strs, err := p.parseClassSetAtom()
	if err != nil {
		return nil, err
	}
	v := newClassSetValue()
	switch kind {
	case classAtomSet:
		v.set.addSet(set)
		return v, nil
	case classAtomStringSet:
		v.set.addSet(set)
		for _, s := range strs {
			v.addString(s)
		}
		return v, nil
	case classAtomStrings:
		for _, s := range strs {
			v.addString(s)
		}
		return v, nil
	}

	// A single `-` between two atoms is a range, exactly as under `u`. A
	// doubled one was handled by the caller as the difference operator.
	if p.peek() == '-' && p.peekAt(1) != '-' && p.peekAt(1) != ']' && p.peekAt(1) != -1 {
		p.pos++
		hi, hiKind, _, _, err := p.parseClassSetAtom()
		if err != nil {
			return nil, err
		}
		if hiKind != classAtomChar {
			return nil, p.errorf("a class range cannot end with a class")
		}
		if lo > hi {
			return nil, p.errorf("range out of order in character class")
		}
		v.set.addRange(lo, hi)
		return v, nil
	}
	v.set.addChar(lo)
	return v, nil
}

// classAtomKind says what parseClassSetAtom found.
type classAtomKind uint8

const (
	classAtomChar classAtomKind = iota
	classAtomSet
	classAtomStrings
	// classAtomStringSet is a property of strings, which contributes both code
	// points and sequences.
	classAtomStringSet
)

// parseClassSetAtom parses one atom of a `v`-mode class.
//
// It differs from the `u`-mode form in two ways: \q{} is accepted, and the
// characters the set syntax uses must be escaped rather than taken literally,
// so that adding an operator later cannot change what an existing pattern
// means.
func (p *parser) parseClassSetAtom() (rune, classAtomKind, *charSet, [][]rune, error) {
	if p.peek() != '\\' {
		c := p.peek()
		// The punctuators the set syntax reserves, and the doubled ones it
		// reserves for future operators, have to be written escaped.
		if isClassSetSyntaxChar(c) {
			return 0, 0, nil, nil, p.errorf("%q must be escaped in a v-mode class", string(c))
		}
		if isClassSetReservedDouble(c) && p.peekAt(1) == c {
			return 0, 0, nil, nil, p.errorf("%q is reserved in a v-mode class", string([]rune{c, c}))
		}
		p.pos++
		if utf16.IsSurrogate(c) && !p.atEnd() {
			if combined := utf16.DecodeRune(c, p.peek()); combined != utf8.RuneError {
				p.pos++
				c = combined
			}
		}
		return c, classAtomChar, nil, nil, nil
	}

	p.pos++ // consume '\'
	if p.atEnd() {
		return 0, 0, nil, nil, p.errorf("\\ at end of pattern")
	}
	switch c := p.peek(); c {
	case 'd', 'D', 'w', 'W', 's', 'S':
		p.pos++
		return 0, classAtomSet, p.shorthandClass(c), nil, nil
	case 'p', 'P':
		p.pos++
		negated := c == 'P'
		name, err := p.readPropertyName()
		if err != nil {
			return 0, 0, nil, nil, err
		}
		if v, ok := unicodeStringPropertyValue(name); ok {
			// There is no sensible "every string but these", so a property of
			// strings cannot be negated.
			if negated {
				return 0, 0, nil, nil, p.errorf("a property of strings cannot be negated")
			}
			return 0, classAtomStringSet, v.set, v.strings, nil
		}
		set, ok := unicodeClass(name, negated)
		if !ok {
			return 0, 0, nil, nil, p.errorf("unknown Unicode property %q", name)
		}
		return 0, classAtomSet, set, nil, nil
	case 'q':
		p.pos++
		strs, err := p.parseClassStrings()
		return 0, classAtomStrings, nil, strs, err
	case 'b':
		p.pos++
		return '\b', classAtomChar, nil, nil, nil
	}
	c, err := p.parseCharEscape()
	return c, classAtomChar, nil, nil, err
}

// parseClassStrings parses \q{alt|alt|...}.
func (p *parser) parseClassStrings() ([][]rune, error) {
	if !p.eat('{') {
		return nil, p.errorf("\\q must be followed by \"{\"")
	}
	var out [][]rune
	cur := []rune{}
	for {
		if p.atEnd() {
			return nil, p.errorf("unterminated \\q")
		}
		switch p.peek() {
		case '}':
			p.pos++
			out = append(out, cur)
			return out, nil
		case '|':
			p.pos++
			out = append(out, cur)
			cur = []rune{}
			continue
		}
		c, kind, _, _, err := p.parseClassSetAtom()
		if err != nil {
			return nil, err
		}
		if kind != classAtomChar {
			return nil, p.errorf("a class string may only contain characters")
		}
		cur = append(cur, c)
	}
}

// isClassSetSyntaxChar reports the characters a v-mode class reserves.
func isClassSetSyntaxChar(r rune) bool {
	switch r {
	case '(', ')', '[', '{', '}', '/', '-', '|':
		return true
	}
	return false
}

// isClassSetReservedDouble reports the punctuators reserved when doubled, which
// exist so that a future operator cannot change what a pattern written today
// means.
func isClassSetReservedDouble(r rune) bool {
	switch r {
	case '!', '#', '$', '%', '*', '+', ',', '.', ':', ';', '<', '=', '>',
		'?', '@', '^', '`', '~':
		return true
	}
	return false
}

// unicodeStringPropertyValue decodes a property of strings.
//
// These exist only under the v flag, because a class that can match more than
// one character is the thing v adds. \p{RGI_Emoji} is the reason: the set of
// emoji a platform should render is a set of sequences, not of code points, and
// no amount of character-class notation expresses it.
func unicodeStringPropertyValue(name string) (*classSetValue, bool) {
	// RGI_Emoji is by definition the union of the others, so it is assembled
	// rather than stored a second time.
	if name == "RGI_Emoji" {
		v := newClassSetValue()
		for _, part := range rgiEmojiParts {
			p, ok := unicodeStringPropertyValue(part)
			if !ok {
				return nil, false
			}
			v.union(p)
		}
		return v, true
	}
	off, ok := unicodeStringProperties[name]
	if !ok {
		return nil, false
	}
	v := newClassSetValue()
	p := int(off)
	n, p := decodeStringVarint(p)
	for i := uint32(0); i < n; i++ {
		var length uint32
		length, p = decodeStringVarint(p)
		seq := make([]rune, length)
		for j := range seq {
			var c uint32
			c, p = decodeStringVarint(p)
			seq[j] = rune(c)
		}
		// A one-character sequence is a code point, which is what makes
		// [\p{Basic_Emoji}] hold both kinds without distinguishing them.
		v.addString(seq)
	}
	v.set.normalize()
	return v, true
}

// rgiEmojiParts are the properties RGI_Emoji is defined as the union of.
var rgiEmojiParts = []string{
	"Basic_Emoji", "Emoji_Keycap_Sequence", "RGI_Emoji_Flag_Sequence",
	"RGI_Emoji_Modifier_Sequence", "RGI_Emoji_Tag_Sequence",
	"RGI_Emoji_ZWJ_Sequence",
}

// decodeStringVarint reads one varint from the string-property table.
func decodeStringVarint(p int) (uint32, int) {
	var v uint32
	for shift := 0; ; shift += 5 {
		d := digitValues[unicodeStringPropertyData[p]]
		p++
		v |= uint32(d&31) << shift
		if d&32 == 0 {
			return v, p
		}
	}
}
