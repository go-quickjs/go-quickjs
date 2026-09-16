package regexp

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// SyntaxError reports a malformed pattern.
type SyntaxError struct {
	Msg     string
	Pattern string
	Pos     int
}

func (e *SyntaxError) Error() string {
	return fmt.Sprintf("invalid regular expression: /%s/: %s", e.Pattern, e.Msg)
}

// parser turns pattern text into a syntax tree.
//
// The pattern is scanned as runes rather than UTF-16 units, since a pattern
// comes from source text that is already well formed. Only the subject string
// needs the code-unit treatment.
type parser struct {
	src   []rune
	pos   int
	flags Flags

	// groupCount counts capturing groups, which the tree refers to by index.
	groupCount int
	groupNames map[string]int
	// namedRefs records forward references to named groups, resolved once the
	// whole pattern is parsed and every name is known.
	namedRefs []*nodeBackref
	// maxBackref is the largest numeric backreference seen, checked at the end
	// because \1 may legally precede the group it names.
	maxBackref int
}

// parse builds the syntax tree for a pattern.
func parse(pattern string, flags Flags) (node, int, map[string]int, error) {
	p := &parser{
		src:        []rune(pattern),
		flags:      flags,
		groupNames: make(map[string]int),
	}
	// Named groups have to be discovered before the body is parsed, because a
	// reference may precede its definition and because the presence of any
	// named group changes how \k is treated.
	if err := p.scanGroupNames(); err != nil {
		return nil, 0, nil, err
	}

	n, err := p.parseAlternation()
	if err != nil {
		return nil, 0, nil, err
	}
	if p.pos < len(p.src) {
		// The only way to stop early is an unbalanced ')'.
		return nil, 0, nil, p.errorf("unmatched ')'")
	}
	if p.maxBackref > p.groupCount {
		// A reference to a group that does not exist is an error under the u
		// flag and an octal escape otherwise, which scanEscape already handled;
		// reaching here means the strict reading applies.
		if p.flags&FlagUnicode != 0 {
			return nil, 0, nil, p.errorf("invalid backreference \\%d", p.maxBackref)
		}
	}
	for _, ref := range p.namedRefs {
		idx, ok := p.groupNames[ref.name]
		if !ok {
			return nil, 0, nil, p.errorf("invalid named backreference \\k<%s>", ref.name)
		}
		ref.index = idx
	}
	return n, p.groupCount, p.groupNames, nil
}

func (p *parser) errorf(format string, args ...any) error {
	return &SyntaxError{
		Msg:     fmt.Sprintf(format, args...),
		Pattern: string(p.src),
		Pos:     p.pos,
	}
}

func (p *parser) atEnd() bool { return p.pos >= len(p.src) }

func (p *parser) peek() rune {
	if p.atEnd() {
		return -1
	}
	return p.src[p.pos]
}

func (p *parser) peekAt(n int) rune {
	if p.pos+n >= len(p.src) {
		return -1
	}
	return p.src[p.pos+n]
}

func (p *parser) next() rune {
	r := p.src[p.pos]
	p.pos++
	return r
}

func (p *parser) eat(r rune) bool {
	if p.peek() == r {
		p.pos++
		return true
	}
	return false
}

// scanGroupNames makes a pass over the pattern recording every (?<name> group,
// so that a backreference can be resolved wherever it appears.
func (p *parser) scanGroupNames() error {
	depth := 0
	idx := 0
	inClass := false
	for i := 0; i < len(p.src); i++ {
		switch p.src[i] {
		case '\\':
			i++ // skip whatever is escaped
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '(':
			if inClass {
				continue
			}
			depth++
			if i+1 < len(p.src) && p.src[i+1] == '?' {
				// Only (?<name> is a capturing group; (?<= and (?<! are
				// lookbehind, and (?: (?= (?! are not capturing.
				if i+2 < len(p.src) && p.src[i+2] == '<' &&
					i+3 < len(p.src) && p.src[i+3] != '=' && p.src[i+3] != '!' {
					j := i + 3
					var name strings.Builder
					for j < len(p.src) && p.src[j] != '>' {
						name.WriteRune(p.src[j])
						j++
					}
					idx++
					// A duplicate name would make match.groups ambiguous, so
					// it is rejected rather than resolved to one of them.
					if _, dup := p.groupNames[name.String()]; dup {
						return p.errorf("duplicate group name %q", name.String())
					}
					p.groupNames[name.String()] = idx
				}
				continue
			}
			idx++
		case ')':
			if !inClass {
				depth--
			}
		}
	}
	return nil
}

// parseAlternation parses a sequence of alternatives separated by '|'.
func (p *parser) parseAlternation() (node, error) {
	first, err := p.parseSequence()
	if err != nil {
		return nil, err
	}
	if p.peek() != '|' {
		return first, nil
	}
	alts := []node{first}
	for p.eat('|') {
		n, err := p.parseSequence()
		if err != nil {
			return nil, err
		}
		alts = append(alts, n)
	}
	return nodeAlt{alts: alts}, nil
}

// parseSequence parses terms until an alternation bar or a closing paren.
func (p *parser) parseSequence() (node, error) {
	var items []node
	for !p.atEnd() && p.peek() != '|' && p.peek() != ')' {
		n, err := p.parseTerm()
		if err != nil {
			return nil, err
		}
		if n == nil {
			continue
		}
		items = append(items, n)
	}
	switch len(items) {
	case 0:
		return nodeEmpty{}, nil
	case 1:
		return items[0], nil
	}
	return nodeSeq{items: items}, nil
}

// parseTerm parses one atom with its quantifier, if any.
func (p *parser) parseTerm() (node, error) {
	atom, quantifiable, err := p.parseAtom()
	if err != nil {
		return nil, err
	}
	if atom == nil {
		return nil, nil
	}

	min, max, ok, err := p.parseQuantifier()
	if err != nil {
		return nil, err
	}
	if !ok {
		return atom, nil
	}
	if !quantifiable {
		// A quantifier on an assertion is an error under the u flag; in sloppy
		// mode a lookahead may be quantified, which is a legacy allowance.
		if p.flags&FlagUnicode != 0 {
			return nil, p.errorf("nothing to repeat")
		}
		if _, isLook := atom.(nodeLook); !isLook {
			return nil, p.errorf("nothing to repeat")
		}
	}
	greedy := !p.eat('?')
	// A quantifier applies to an atom, and a quantified atom is not one: x{1}*
	// and x{1}{2} have no reading. The punctuation forms are caught when the
	// next term tries to start with them, but a brace would otherwise be taken
	// for a literal.
	if p.quantifierFollows() {
		return nil, p.errorf("nothing to repeat")
	}
	return nodeRepeat{item: atom, min: min, max: max, greedy: greedy}, nil
}

// quantifierFollows reports whether a quantifier starts at the current
// position, without consuming it.
func (p *parser) quantifierFollows() bool {
	save := p.pos
	_, _, ok, err := p.parseQuantifier()
	p.pos = save
	return ok || err != nil
}

// parseQuantifier reads *, +, ? or {n,m}, reporting whether one was present.
func (p *parser) parseQuantifier() (min, max int, ok bool, err error) {
	switch p.peek() {
	case '*':
		p.pos++
		return 0, -1, true, nil
	case '+':
		p.pos++
		return 1, -1, true, nil
	case '?':
		p.pos++
		return 0, 1, true, nil
	case '{':
		// A brace that does not form a valid quantifier is a literal in sloppy
		// mode, so the position is restored rather than reported.
		save := p.pos
		p.pos++
		lo, okLo := p.parseDecimal()
		if !okLo {
			p.pos = save
			return 0, 0, false, nil
		}
		hi := lo
		if p.eat(',') {
			if p.peek() == '}' {
				hi = -1
			} else {
				var okHi bool
				hi, okHi = p.parseDecimal()
				if !okHi {
					p.pos = save
					return 0, 0, false, nil
				}
			}
		}
		if !p.eat('}') {
			p.pos = save
			return 0, 0, false, nil
		}
		if hi >= 0 && lo > hi {
			return 0, 0, false, p.errorf("numbers out of order in {} quantifier")
		}
		return lo, hi, true, nil
	}
	return 0, 0, false, nil
}

func (p *parser) parseDecimal() (int, bool) {
	start := p.pos
	for !p.atEnd() && p.peek() >= '0' && p.peek() <= '9' {
		p.pos++
	}
	if p.pos == start {
		return 0, false
	}
	n, err := strconv.Atoi(string(p.src[start:p.pos]))
	if err != nil {
		// A count too large to represent is treated as unbounded, which is the
		// only sensible reading and what other engines do.
		return 1 << 30, true
	}
	return n, true
}

// parseAtom parses a single element, reporting whether a quantifier may follow.
func (p *parser) parseAtom() (n node, quantifiable bool, err error) {
	switch r := p.peek(); r {
	case '^':
		p.pos++
		return nodeAssert{kind: assertStart, multiline: p.flags&FlagMultiline != 0}, false, nil
	case '$':
		p.pos++
		return nodeAssert{kind: assertEnd, multiline: p.flags&FlagMultiline != 0}, false, nil
	case '.':
		p.pos++
		return nodeAny{dotAll: p.flags&FlagDotAll != 0}, true, nil
	case '(':
		return p.parseGroup()
	case '[':
		if p.flags&FlagUnicodeSets != 0 {
			// The v flag makes a class a set expression rather than a flat
			// list, and one that can denote strings as well as code points.
			n, err := p.parseClassSet()
			if err != nil {
				return nil, false, err
			}
			return n, true, nil
		}
		set, err := p.parseClass()
		if err != nil {
			return nil, false, err
		}
		return nodeClass{set: set}, true, nil
	case '\\':
		return p.parseEscape()
	case '*', '+', '?':
		return nil, false, p.errorf("nothing to repeat")
	case ')':
		return nil, false, nil
	case '{':
		// A lone brace is a literal, but only when it cannot start a
		// quantifier: /{a}/ is the three characters, while /{1}/ is a
		// quantifier with nothing to apply to.
		if p.flags&FlagUnicode != 0 || p.quantifierFollows() {
			return nil, false, p.errorf("nothing to repeat")
		}
		p.pos++
		return nodeChar{r: '{'}, true, nil
	case ']', '}':
		if p.flags&FlagUnicode != 0 {
			return nil, false, p.errorf("lone quantifier brackets")
		}
		p.pos++
		return nodeChar{r: r}, true, nil
	}

	r := p.next()
	// A surrogate pair in the pattern is one character under the u flag.
	if p.flags&FlagUnicode != 0 && utf16.IsSurrogate(r) && !p.atEnd() {
		if combined := utf16.DecodeRune(r, p.peek()); combined != utf8.RuneError {
			p.pos++
			r = combined
		}
	}
	return nodeChar{r: r}, true, nil
}

// parseGroup parses a parenthesized construct.
func (p *parser) parseGroup() (node, bool, error) {
	p.pos++ // consume '('

	if p.eat('?') {
		switch {
		case p.eat(':'):
			inner, err := p.parseAlternation()
			if err != nil {
				return nil, false, err
			}
			if !p.eat(')') {
				return nil, false, p.errorf("unterminated group")
			}
			return nodeGroup{item: inner}, true, nil

		case isModifierStart(p.peek()):
			return p.parseModifierGroup()

		case p.eat('='):
			return p.finishLook(false, false)
		case p.eat('!'):
			return p.finishLook(false, true)

		case p.peek() == '<' && p.peekAt(1) == '=':
			p.pos += 2
			return p.finishLook(true, false)
		case p.peek() == '<' && p.peekAt(1) == '!':
			p.pos += 2
			return p.finishLook(true, true)

		case p.eat('<'):
			// A named capturing group.
			name, err := p.parseGroupName()
			if err != nil {
				return nil, false, err
			}
			p.groupCount++
			idx := p.groupCount
			inner, err := p.parseAlternation()
			if err != nil {
				return nil, false, err
			}
			if !p.eat(')') {
				return nil, false, p.errorf("unterminated group")
			}
			return nodeGroup{item: inner, index: idx, name: name}, true, nil
		}
		return nil, false, p.errorf("invalid group")
	}

	p.groupCount++
	idx := p.groupCount
	inner, err := p.parseAlternation()
	if err != nil {
		return nil, false, err
	}
	if !p.eat(')') {
		return nil, false, p.errorf("unterminated group")
	}
	return nodeGroup{item: inner, index: idx}, true, nil
}

// finishLook parses the body of a lookaround, whose opener has been consumed.
func (p *parser) finishLook(behind, negate bool) (node, bool, error) {
	inner, err := p.parseAlternation()
	if err != nil {
		return nil, false, err
	}
	if !p.eat(')') {
		return nil, false, p.errorf("unterminated group")
	}
	// A lookaround is zero-width, so a quantifier on it is meaningless; sloppy
	// mode allows it on a lookahead for compatibility, which parseTerm handles.
	return nodeLook{item: inner, behind: behind, negate: negate}, false, nil
}

func (p *parser) parseGroupName() (string, error) {
	var sb strings.Builder
	first := true
	for !p.atEnd() && p.peek() != '>' {
		r := p.next()
		// A group name is an identifier, so that it can be read back as
		// match.groups.name without quoting.
		if first && !isGroupNameStart(r) {
			return "", p.errorf("a group name cannot start with %q", string(r))
		}
		if !first && !isGroupNamePart(r) {
			return "", p.errorf("%q is not valid in a group name", string(r))
		}
		first = false
		sb.WriteRune(r)
	}
	if !p.eat('>') {
		return "", p.errorf("unterminated group name")
	}
	if sb.Len() == 0 {
		return "", p.errorf("empty group name")
	}
	return sb.String(), nil
}

// isGroupNameStart and isGroupNamePart mirror the identifier rules, which is
// what a group name has to satisfy.
func isGroupNameStart(r rune) bool {
	return r == '$' || r == '_' || unicode.IsLetter(r)
}

func isGroupNamePart(r rune) bool {
	return isGroupNameStart(r) || r == 0x200C || r == 0x200D ||
		unicode.IsDigit(r) || unicode.In(r, unicode.Mn, unicode.Mc, unicode.Pc)
}

// parseEscape parses a backslash sequence outside a character class.
func (p *parser) parseEscape() (node, bool, error) {
	p.pos++ // consume '\'
	if p.atEnd() {
		return nil, false, p.errorf("\\ at end of pattern")
	}

	switch r := p.peek(); r {
	case 'b':
		p.pos++
		return nodeAssert{kind: assertWordBoundary}, false, nil
	case 'B':
		p.pos++
		return nodeAssert{kind: assertNotWordBoundary}, false, nil

	case 'd', 'D', 'w', 'W', 's', 'S':
		p.pos++
		return nodeClass{set: p.shorthandClass(r)}, true, nil

	case 'p', 'P':
		if p.flags&FlagUnicode == 0 {
			// Without the u flag these are just the letters.
			p.pos++
			return nodeChar{r: r}, true, nil
		}
		p.pos++
		set, err := p.parseUnicodeProperty(r == 'P')
		if err != nil {
			return nil, false, err
		}
		return nodeClass{set: set}, true, nil

	case 'k':
		p.pos++
		if p.eat('<') {
			name, err := p.parseGroupName()
			if err != nil {
				return nil, false, err
			}
			ref := &nodeBackref{name: name}
			p.namedRefs = append(p.namedRefs, ref)
			return ref, true, nil
		}
		if len(p.groupNames) > 0 || p.flags&FlagUnicode != 0 {
			return nil, false, p.errorf("invalid \\k escape")
		}
		return nodeChar{r: 'k'}, true, nil

	case '1', '2', '3', '4', '5', '6', '7', '8', '9':
		n, _ := p.parseDecimal()
		if n > p.maxBackref {
			p.maxBackref = n
		}
		return &nodeBackref{index: n}, true, nil
	}

	r, err := p.parseCharEscape()
	if err != nil {
		return nil, false, err
	}
	return nodeChar{r: r}, true, nil
}

// shorthandClass returns the set a single-letter class escape denotes.
func (p *parser) shorthandClass(r rune) *charSet {
	var base *charSet
	switch r {
	case 'd':
		base = classDigit
	case 'D':
		base = classNotDigit
	case 'w':
		base = classWord
	case 'W':
		base = classNotWord
	case 's':
		base = classSpace
	default:
		base = classNotSpace
	}
	// \w under both i and u additionally matches the Kelvin sign and the long
	// s, because those fold to k and s. Copying the set keeps the shared one
	// free of the flag.
	if p.flags&FlagIgnoreCase != 0 && (r == 'w' || r == 'W') {
		cp := *base
		cp.foldCase = true
		return &cp
	}
	return base
}

// parseUnicodeProperty parses the body of a \p{...} escape.
func (p *parser) parseUnicodeProperty(negate bool) (*charSet, error) {
	if !p.eat('{') {
		return nil, p.errorf("invalid \\p escape")
	}
	var sb strings.Builder
	for !p.atEnd() && p.peek() != '}' {
		sb.WriteRune(p.next())
	}
	if !p.eat('}') {
		return nil, p.errorf("unterminated \\p escape")
	}
	set, ok := unicodeClass(sb.String(), negate)
	if !ok {
		return nil, p.errorf("unknown Unicode property %q", sb.String())
	}
	return set, nil
}

// parseCharEscape resolves an escape that denotes a single character.
func (p *parser) parseCharEscape() (rune, error) {
	r := p.next()
	switch r {
	case 'n':
		return '\n', nil
	case 'r':
		return '\r', nil
	case 't':
		return '\t', nil
	case 'v':
		return '\v', nil
	case 'f':
		return '\f', nil
	case '0':
		// \0 is NUL unless a digit follows, which makes it a legacy octal
		// escape outside unicode mode.
		if p.peek() >= '0' && p.peek() <= '9' && p.flags&FlagUnicode == 0 {
			p.pos--
			return p.parseOctal(), nil
		}
		return 0, nil
	case 'c':
		// A control escape: \cA is U+0001.
		if c := p.peek(); (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			p.pos++
			return c % 32, nil
		}
		if p.flags&FlagUnicode != 0 {
			return 0, p.errorf("invalid \\c escape")
		}
		// In sloppy mode a stray \c is a literal backslash followed by c.
		return '\\', nil
	case 'x':
		if v, ok := p.parseHexDigits(2); ok {
			return v, nil
		}
		if p.flags&FlagUnicode != 0 {
			return 0, p.errorf("invalid \\x escape")
		}
		return 'x', nil
	case 'u':
		return p.parseUnicodeEscape()
	}

	if r >= '1' && r <= '7' && p.flags&FlagUnicode == 0 {
		p.pos--
		return p.parseOctal(), nil
	}
	if p.flags&FlagUnicode != 0 && isIdentifierRune(r) {
		// Under the u flag only a fixed set of escapes is valid, so an
		// unrecognized one is an error rather than the literal character.
		return 0, p.errorf("invalid escape \\%c", r)
	}
	return r, nil
}

// parseUnicodeEscape reads \uXXXX or \u{...}, combining a surrogate pair when
// the u flag is set.
func (p *parser) parseUnicodeEscape() (rune, error) {
	if p.eat('{') {
		if p.flags&FlagUnicode == 0 {
			// Without the u flag, \u{2} is "u" repeated twice, so the brace is
			// not part of the escape.
			p.pos--
			return 'u', nil
		}
		start := p.pos
		for !p.atEnd() && p.peek() != '}' {
			p.pos++
		}
		v, err := strconv.ParseInt(string(p.src[start:p.pos]), 16, 32)
		if err != nil || v > 0x10FFFF {
			return 0, p.errorf("invalid Unicode escape")
		}
		if !p.eat('}') {
			return 0, p.errorf("unterminated Unicode escape")
		}
		return rune(v), nil
	}

	v, ok := p.parseHexDigits(4)
	if !ok {
		if p.flags&FlagUnicode != 0 {
			return 0, p.errorf("invalid Unicode escape")
		}
		return 'u', nil
	}
	// Under the u flag a leading surrogate joins with a following one.
	if p.flags&FlagUnicode != 0 && utf16.IsSurrogate(v) &&
		p.peek() == '\\' && p.peekAt(1) == 'u' {
		save := p.pos
		p.pos += 2
		if lo, ok := p.parseHexDigits(4); ok {
			if combined := utf16.DecodeRune(v, lo); combined != utf8.RuneError {
				return combined, nil
			}
		}
		p.pos = save
	}
	return v, nil
}

func (p *parser) parseHexDigits(n int) (rune, bool) {
	if p.pos+n > len(p.src) {
		return 0, false
	}
	v, err := strconv.ParseInt(string(p.src[p.pos:p.pos+n]), 16, 32)
	if err != nil {
		return 0, false
	}
	p.pos += n
	return rune(v), true
}

// parseOctal reads a legacy octal escape of up to three digits.
func (p *parser) parseOctal() rune {
	v := rune(0)
	for i := 0; i < 3 && !p.atEnd(); i++ {
		c := p.peek()
		if c < '0' || c > '7' {
			break
		}
		next := v*8 + (c - '0')
		if next > 0xFF {
			break
		}
		v = next
		p.pos++
	}
	return v
}

func isIdentifierRune(r rune) bool {
	return r == '_' || (r >= '0' && r <= '9') ||
		(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// parseClass parses a bracketed character class.
func (p *parser) parseClass() (*charSet, error) {
	p.pos++ // consume '['
	set := newCharSet()
	set.negated = p.eat('^')
	if p.flags&FlagIgnoreCase != 0 {
		set.foldCase = true
	}

	for {
		if p.atEnd() {
			return nil, p.errorf("unterminated character class")
		}
		if p.eat(']') {
			set.normalize()
			return set, nil
		}

		lo, isClass, classSet, err := p.parseClassAtom()
		if err != nil {
			return nil, err
		}
		if isClass {
			// A shorthand cannot be an endpoint of a range.
			if p.peek() == '-' && p.peekAt(1) != ']' && p.flags&FlagUnicode != 0 {
				return nil, p.errorf("invalid character class range")
			}
			set.addSet(classSet)
			continue
		}

		// A '-' makes a range unless it is the last character before ']'.
		if p.peek() == '-' && p.peekAt(1) != ']' && p.peekAt(1) != -1 {
			p.pos++
			hi, hiIsClass, _, err := p.parseClassAtom()
			if err != nil {
				return nil, err
			}
			if hiIsClass {
				if p.flags&FlagUnicode != 0 {
					return nil, p.errorf("invalid character class range")
				}
				// In sloppy mode the dash is literal and the shorthand stands
				// on its own.
				set.addChar(lo)
				set.addChar('-')
				continue
			}
			if lo > hi {
				return nil, p.errorf("range out of order in character class")
			}
			set.addRange(lo, hi)
			continue
		}
		set.addChar(lo)
	}
}

// parseClassAtom parses one element of a character class, which is either a
// single character or a shorthand set.
func (p *parser) parseClassAtom() (r rune, isClass bool, set *charSet, err error) {
	if p.peek() != '\\' {
		c := p.next()
		if p.flags&FlagUnicode != 0 && utf16.IsSurrogate(c) && !p.atEnd() {
			if combined := utf16.DecodeRune(c, p.peek()); combined != utf8.RuneError {
				p.pos++
				c = combined
			}
		}
		return c, false, nil, nil
	}

	p.pos++ // consume '\'
	if p.atEnd() {
		return 0, false, nil, p.errorf("\\ at end of pattern")
	}
	switch c := p.peek(); c {
	case 'd', 'D', 'w', 'W', 's', 'S':
		p.pos++
		return 0, true, p.shorthandClass(c), nil
	case 'p', 'P':
		if p.flags&FlagUnicode != 0 {
			p.pos++
			s, err := p.parseUnicodeProperty(c == 'P')
			if err != nil {
				return 0, false, nil, err
			}
			return 0, true, s, nil
		}
	case 'b':
		// Inside a class \b is a backspace rather than a word boundary.
		p.pos++
		return '\b', false, nil, nil
	case '-':
		p.pos++
		return '-', false, nil, nil
	}
	c, err := p.parseCharEscape()
	return c, false, nil, err
}

// isModifierStart reports whether a character can begin an inline modifier.
func isModifierStart(r rune) bool {
	return r == 'i' || r == 'm' || r == 's' || r == '-'
}

// parseModifierGroup parses `(?flags:...)` and `(?flags-flags:...)`.
//
// The flags apply only to what the group contains, so the parser's own flags
// are changed for the inner parse and restored afterwards. Everything that
// depends on them -- whether `.` matches a newline, whether `^` is per-line,
// whether a literal folds case -- is decided while parsing or compiling that
// subtree, so restoring is enough to confine them.
func (p *parser) parseModifierGroup() (node, bool, error) {
	var add, remove Flags
	seen := map[rune]bool{}

	readFlags := func(into *Flags) error {
		for {
			r := p.peek()
			var f Flags
			switch r {
			case 'i':
				f = FlagIgnoreCase
			case 'm':
				f = FlagMultiline
			case 's':
				f = FlagDotAll
			default:
				return nil
			}
			// A flag may appear once across both halves: `(?i-i:)` is as much a
			// contradiction as `(?ii:)` is a repeat.
			if seen[r] {
				return p.errorf("duplicate modifier %q", string(r))
			}
			seen[r] = true
			*into |= f
			p.pos++
		}
	}

	if err := readFlags(&add); err != nil {
		return nil, false, err
	}
	if p.eat('-') {
		if err := readFlags(&remove); err != nil {
			return nil, false, err
		}
		if remove == 0 {
			return nil, false, p.errorf("a modifier group must name a flag to remove")
		}
	} else if add == 0 {
		return nil, false, p.errorf("a modifier group must name a flag")
	}
	if !p.eat(':') {
		return nil, false, p.errorf("a modifier group must be followed by \":\"")
	}

	saved := p.flags
	p.flags = (p.flags | add) &^ remove
	inner, err := p.parseAlternation()
	innerFlags := p.flags
	p.flags = saved
	if err != nil {
		return nil, false, err
	}
	if !p.eat(')') {
		return nil, false, p.errorf("unterminated group")
	}
	return nodeModifier{flags: innerFlags, item: inner}, true, nil
}
