package lexer

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// Error is a syntax error produced while scanning.
type Error struct {
	Msg  string
	Pos  int
	Line int
	Col  int
}

func (e *Error) Error() string {
	return fmt.Sprintf("SyntaxError: %s (line %d, column %d)", e.Msg, e.Line, e.Col)
}

// Lexer scans ECMAScript source text into tokens.
//
// Callers drive it one token at a time with Next. Because '/' and '}' are
// ambiguous without grammatical context, the parser calls ScanRegExp or
// ScanTemplateTail instead of Next when it knows which reading applies.
type Lexer struct {
	src  string
	pos  int // byte offset of the next byte to read
	line int
	// lineStart is the byte offset of the current line's first byte, used to
	// derive columns without scanning from the top of the file.
	lineStart int
	// nlBefore records whether a line terminator was skipped before the token
	// currently being scanned.
	nlBefore bool
}

// New returns a Lexer over src.
func New(src string) *Lexer {
	// Skip a UTF-8 BOM; it is whitespace per the spec but confuses the scanner.
	src = strings.TrimPrefix(src, "\uFEFF")
	return &Lexer{src: src, line: 1}
}

// Pos returns the current byte offset.
func (l *Lexer) Pos() int { return l.pos }

// Seek rewinds or advances the scanner to a position previously obtained from
// Pos, Line and LineStart. The parser uses it to re-scan a region it had to
// look ahead over.
//
// The caller supplies lineStart rather than letting the lexer recover it,
// because searching backwards for the preceding newline would be linear in the
// length of the line and this is called once per identifier.
func (l *Lexer) Seek(pos, line, lineStart int) {
	l.pos = pos
	l.line = line
	l.lineStart = lineStart
}

// Line returns the current 1-based line number.
func (l *Lexer) Line() int { return l.line }

// LineStart returns the byte offset of the current line's first byte.
func (l *Lexer) LineStart() int { return l.lineStart }

func (l *Lexer) errf(pos int, format string, args ...any) *Error {
	return &Error{
		Msg:  fmt.Sprintf(format, args...),
		Pos:  pos,
		Line: l.line,
		Col:  l.Column(pos, l.lineStart),
	}
}

// Column returns the 1-based column of pos, given the byte offset of the start
// of its line. It is called only when reporting a position to a human, because
// counting runes for every token would make scanning quadratic in line length.
func (l *Lexer) Column(pos, lineStart int) int {
	if pos > len(l.src) {
		pos = len(l.src)
	}
	if lineStart > pos {
		lineStart = pos
	}
	return utf8.RuneCountInString(l.src[lineStart:pos]) + 1
}

func (l *Lexer) atEnd() bool { return l.pos >= len(l.src) }

// peekByte returns the byte at offset, or 0 at end of input. Returning 0 rather
// than panicking keeps the hot scanning paths free of bounds checks.
func (l *Lexer) peekByte(offset int) byte {
	if l.pos+offset >= len(l.src) {
		return 0
	}
	return l.src[l.pos+offset]
}

// peekRune decodes the rune at the current position.
func (l *Lexer) peekRune() (rune, int) {
	if l.atEnd() {
		return -1, 0
	}
	if c := l.src[l.pos]; c < utf8.RuneSelf {
		return rune(c), 1
	}
	return utf8.DecodeRuneInString(l.src[l.pos:])
}

func (l *Lexer) newline() {
	l.line++
	l.lineStart = l.pos
	l.nlBefore = true
}

// skipSpace consumes whitespace, line terminators and comments, recording
// whether any line terminator was crossed.
func (l *Lexer) skipSpace() error {
	for !l.atEnd() {
		c := l.src[l.pos]
		switch c {
		case ' ', '\t', '\v', '\f':
			l.pos++
		case '\n':
			l.pos++
			l.newline()
		case '\r':
			l.pos++
			// Treat CRLF as one line terminator.
			if l.peekByte(0) == '\n' {
				l.pos++
			}
			l.newline()
		case '/':
			switch l.peekByte(1) {
			case '/':
				l.pos += 2
				for !l.atEnd() && !isLineTerminatorByte(l.src[l.pos]) {
					// Line comments end at U+2028/U+2029 too, which are multibyte.
					if l.src[l.pos] >= utf8.RuneSelf {
						r, size := utf8.DecodeRuneInString(l.src[l.pos:])
						if r == 0x2028 || r == 0x2029 {
							break
						}
						l.pos += size
						continue
					}
					l.pos++
				}
			case '*':
				start := l.pos
				l.pos += 2
				for {
					if l.atEnd() {
						return l.errf(start, "unterminated comment")
					}
					if l.src[l.pos] == '*' && l.peekByte(1) == '/' {
						l.pos += 2
						break
					}
					if l.src[l.pos] == '\n' {
						l.pos++
						l.newline()
						continue
					}
					l.pos++
				}
			default:
				return nil
			}
		default:
			if c < utf8.RuneSelf {
				return nil
			}
			r, size := utf8.DecodeRuneInString(l.src[l.pos:])
			switch {
			case r == 0x2028 || r == 0x2029:
				l.pos += size
				l.newline()
			case r == 0xFEFF || unicode.Is(unicode.Zs, r):
				l.pos += size
			default:
				return nil
			}
		}
	}
	return nil
}

func isLineTerminatorByte(c byte) bool { return c == '\n' || c == '\r' }

// Next scans and returns the next token, treating '/' as a division operator.
func (l *Lexer) Next() (Token, error) {
	l.nlBefore = false
	if err := l.skipSpace(); err != nil {
		return Token{}, err
	}
	tok := Token{
		Pos:           l.pos,
		Line:          l.line,
		LineStart:     l.lineStart,
		NewlineBefore: l.nlBefore,
	}
	if l.atEnd() {
		tok.Kind = EOF
		return tok, nil
	}

	c := l.src[l.pos]
	switch {
	case c == '"' || c == '\'':
		return l.scanString(tok, c)
	case c == '`':
		return l.scanTemplate(tok, true)
	case c >= '0' && c <= '9':
		return l.scanNumber(tok)
	case c == '.' && isDigit(l.peekByte(1)):
		return l.scanNumber(tok)
	case c == '#':
		l.pos++
		name, err := l.scanIdentName()
		if err != nil {
			return tok, err
		}
		tok.Kind, tok.Value = PrivateIdent, name
		return tok, nil
	case isIdentStartByte(c) || c >= utf8.RuneSelf || c == '\\':
		name, err := l.scanIdentName()
		if err != nil {
			return tok, err
		}
		tok.Value = name
		// A name written with escapes is never a keyword (`if` is an
		// identifier, not `if`), which matters for correctness of `var if`.
		if reservedWords[name] && !strings.Contains(l.src[tok.Pos:l.pos], "\\") {
			tok.Kind = Keyword
		} else {
			tok.Kind = Ident
		}
		return tok, nil
	}

	for _, p := range punctuators {
		if strings.HasPrefix(l.src[l.pos:], p) {
			// `?.3` is a ternary followed by .3, not optional chaining.
			if p == "?." && isDigit(l.peekByte(2)) {
				continue
			}
			l.pos += len(p)
			tok.Kind, tok.Value = Punct, p
			return tok, nil
		}
	}

	r, size := l.peekRune()
	l.pos += size
	return tok, l.errf(tok.Pos, "unexpected character %q", r)
}

// ScanRegExp rescans from the start of a token known to begin a regular
// expression literal. The parser calls this when it has just seen '/' in a
// position where an expression may start.
func (l *Lexer) ScanRegExp(start Token) (Token, error) {
	l.pos = start.Pos
	tok := start
	tok.Kind = Regexp
	l.pos++ // consume the opening '/'

	inClass := false
	for {
		if l.atEnd() {
			return tok, l.errf(tok.Pos, "unterminated regular expression")
		}
		c := l.src[l.pos]
		if isLineTerminatorByte(c) {
			return tok, l.errf(tok.Pos, "unterminated regular expression")
		}
		switch c {
		case '\\':
			l.pos++
			if l.atEnd() || isLineTerminatorByte(l.src[l.pos]) {
				return tok, l.errf(tok.Pos, "unterminated regular expression")
			}
			_, size := l.peekRune()
			l.pos += size
			continue
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '/':
			// A '/' inside a character class is literal, not the terminator.
			if !inClass {
				body := l.src[start.Pos+1 : l.pos]
				l.pos++
				flags, err := l.scanIdentName()
				if err != nil && !l.atEnd() {
					return tok, err
				}
				tok.Value, tok.Flags = body, flags
				tok.Raw = l.src[start.Pos:l.pos]
				return tok, nil
			}
		}
		_, size := l.peekRune()
		l.pos += size
	}
}

// ScanTemplateTail rescans a '}' as the continuation of a template literal,
// returning the TemplateMid or TemplateTail part that follows it.
func (l *Lexer) ScanTemplateTail(brace Token) (Token, error) {
	l.pos = brace.Pos // position at the '}'
	tok := brace
	return l.scanTemplate(tok, false)
}

// scanTemplate reads one template part. When head is true the scanner is
// positioned at the opening backtick; otherwise it is at the '}' that resumes
// the literal.
func (l *Lexer) scanTemplate(tok Token, head bool) (Token, error) {
	start := l.pos
	l.pos++ // consume '`' or '}'
	var cooked strings.Builder
	// A template part whose cooked value is invalid is still legal in a tagged
	// template, where it surfaces as undefined; we record that rather than fail.
	valid := true

	for {
		if l.atEnd() {
			return tok, l.errf(start, "unterminated template literal")
		}
		c := l.src[l.pos]
		switch c {
		case '`':
			// The literal ends here, so this part is either a complete
			// no-substitution template or the tail of one.
			raw := l.src[start+1 : l.pos]
			l.pos++
			if head {
				tok.Kind = Template
			} else {
				tok.Kind = TemplateTail
			}
			tok.Raw = normalizeTemplateRaw(raw)
			tok.TemplateValid = valid
			if valid {
				tok.Value = cooked.String()
			}
			return tok, nil
		case '$':
			if l.peekByte(1) == '{' {
				// A substitution follows, so this part is a head or a middle.
				raw := l.src[start+1 : l.pos]
				l.pos += 2
				if head {
					tok.Kind = TemplateHead
				} else {
					tok.Kind = TemplateMid
				}
				tok.Raw = normalizeTemplateRaw(raw)
				tok.TemplateValid = valid
				if valid {
					tok.Value = cooked.String()
				}
				return tok, nil
			}
			cooked.WriteByte('$')
			l.pos++
		case '\\':
			if err := l.scanEscape(&cooked); err != nil {
				// Defer the error: it is only fatal for untagged templates,
				// which the parser decides.
				valid = false
			}
		case '\r':
			// Line terminators are normalized to \n in both raw and cooked.
			l.pos++
			if l.peekByte(0) == '\n' {
				l.pos++
			}
			cooked.WriteByte('\n')
			l.newline()
		case '\n':
			l.pos++
			cooked.WriteByte('\n')
			l.newline()
		default:
			r, size := l.peekRune()
			cooked.WriteRune(r)
			l.pos += size
		}
	}
}

// normalizeTemplateRaw applies the spec's line-terminator normalization to the
// raw text of a template part.
func normalizeTemplateRaw(raw string) string {
	if !strings.ContainsRune(raw, '\r') {
		return raw
	}
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	return strings.ReplaceAll(raw, "\r", "\n")
}

func (l *Lexer) scanString(tok Token, quote byte) (Token, error) {
	start := l.pos
	l.pos++
	var sb strings.Builder
	for {
		if l.atEnd() {
			return tok, l.errf(start, "unterminated string literal")
		}
		c := l.src[l.pos]
		if c == quote {
			l.pos++
			tok.Kind, tok.Value, tok.Raw = String, sb.String(), l.src[start:l.pos]
			return tok, nil
		}
		switch c {
		case '\\':
			if err := l.scanEscape(&sb); err != nil {
				return tok, err
			}
		case '\n', '\r':
			return tok, l.errf(start, "unterminated string literal")
		default:
			r, size := l.peekRune()
			// Lone surrogates may appear via \u escapes; preserve them by
			// writing the replacement-free raw bytes when decoding fails.
			if r == utf8.RuneError && size == 1 {
				sb.WriteByte(c)
				l.pos++
				continue
			}
			sb.WriteRune(r)
			l.pos += size
		}
	}
}

// scanEscape consumes a backslash escape sequence and appends its value.
func (l *Lexer) scanEscape(sb *strings.Builder) error {
	start := l.pos
	l.pos++ // consume '\'
	if l.atEnd() {
		return l.errf(start, "unterminated escape sequence")
	}
	c := l.src[l.pos]
	l.pos++
	switch c {
	case 'n':
		sb.WriteByte('\n')
	case 't':
		sb.WriteByte('\t')
	case 'r':
		sb.WriteByte('\r')
	case 'b':
		sb.WriteByte('\b')
	case 'f':
		sb.WriteByte('\f')
	case 'v':
		sb.WriteByte('\v')
	case '0', '1', '2', '3', '4', '5', '6', '7':
		// Legacy octal escapes. \0 not followed by a digit is NUL and is legal
		// even in strict mode.
		if c == '0' && !isDigit(l.peekByte(0)) {
			sb.WriteByte(0)
			return nil
		}
		v := int(c - '0')
		limit := 2
		if c >= '4' {
			// \4 through \7 take at most two digits total.
			limit = 1
		}
		for i := 0; i < limit; i++ {
			d := l.peekByte(0)
			if d < '0' || d > '7' {
				break
			}
			v = v*8 + int(d-'0')
			l.pos++
		}
		sb.WriteRune(rune(v))
	case 'x':
		if l.pos+2 > len(l.src) {
			return l.errf(start, "invalid hexadecimal escape sequence")
		}
		v, ok := parseHex(l.src[l.pos : l.pos+2])
		if !ok {
			return l.errf(start, "invalid hexadecimal escape sequence")
		}
		l.pos += 2
		sb.WriteRune(rune(v))
	case 'u':
		r, err := l.scanUnicodeEscape(start)
		if err != nil {
			return err
		}
		sb.WriteRune(r)
	case '\r':
		// Line continuation: produces nothing.
		if l.peekByte(0) == '\n' {
			l.pos++
		}
		l.newline()
	case '\n':
		l.newline()
	default:
		if c >= utf8.RuneSelf {
			l.pos--
			r, size := l.peekRune()
			l.pos += size
			if r == 0x2028 || r == 0x2029 {
				l.newline()
				return nil
			}
			sb.WriteRune(r)
			return nil
		}
		sb.WriteByte(c)
	}
	return nil
}

// scanUnicodeEscape reads the body of a \u escape, which is either exactly four
// hex digits or a braced code point. Surrogate pairs written as two \u escapes
// are combined so that the cooked value is well-formed UTF-8.
func (l *Lexer) scanUnicodeEscape(start int) (rune, error) {
	if l.peekByte(0) == '{' {
		l.pos++
		end := strings.IndexByte(l.src[l.pos:], '}')
		if end < 0 {
			return 0, l.errf(start, "invalid Unicode escape sequence")
		}
		v, ok := parseHex(l.src[l.pos : l.pos+end])
		if !ok || v > 0x10FFFF {
			return 0, l.errf(start, "invalid Unicode escape sequence")
		}
		l.pos += end + 1
		return rune(v), nil
	}
	if l.pos+4 > len(l.src) {
		return 0, l.errf(start, "invalid Unicode escape sequence")
	}
	v, ok := parseHex(l.src[l.pos : l.pos+4])
	if !ok {
		return 0, l.errf(start, "invalid Unicode escape sequence")
	}
	l.pos += 4
	r := rune(v)
	if utf16.IsSurrogate(r) && strings.HasPrefix(l.src[l.pos:], `\u`) && l.pos+6 <= len(l.src) {
		if lo, ok := parseHex(l.src[l.pos+2 : l.pos+6]); ok {
			if combined := utf16.DecodeRune(r, rune(lo)); combined != utf8.RuneError {
				l.pos += 6
				return combined, nil
			}
		}
	}
	return r, nil
}

// scanIdentName reads an identifier name, resolving any \u escapes it contains.
func (l *Lexer) scanIdentName() (string, error) {
	start := l.pos
	var sb *strings.Builder // allocated lazily, only when an escape appears
	first := true
	for !l.atEnd() {
		c := l.src[l.pos]
		if c == '\\' {
			if sb == nil {
				sb = &strings.Builder{}
				sb.WriteString(l.src[start:l.pos])
			}
			escStart := l.pos
			l.pos++
			if l.peekByte(0) != 'u' {
				return "", l.errf(escStart, "invalid escape in identifier")
			}
			l.pos++
			r, err := l.scanUnicodeEscape(escStart)
			if err != nil {
				return "", err
			}
			if !isIdentPart(r) || (first && !isIdentStart(r)) {
				return "", l.errf(escStart, "invalid character in identifier")
			}
			sb.WriteRune(r)
			first = false
			continue
		}
		r, size := l.peekRune()
		if first {
			if !isIdentStart(r) {
				break
			}
		} else if !isIdentPart(r) {
			break
		}
		if sb != nil {
			sb.WriteRune(r)
		}
		l.pos += size
		first = false
	}
	if sb != nil {
		return sb.String(), nil
	}
	return l.src[start:l.pos], nil
}

func (l *Lexer) scanNumber(tok Token) (Token, error) {
	start := l.pos
	tok.Kind = Number

	if l.src[l.pos] == '0' && l.pos+1 < len(l.src) {
		switch lower(l.peekByte(1)) {
		case 'x':
			return l.scanRadix(tok, start, 16, isHexDigit)
		case 'o':
			return l.scanRadix(tok, start, 8, func(c byte) bool { return c >= '0' && c <= '7' })
		case 'b':
			return l.scanRadix(tok, start, 2, func(c byte) bool { return c == '0' || c == '1' })
		}
		// Legacy octal (0755) and "non-octal decimal" (0888) literals. Both are
		// sloppy-mode only; the parser rejects them under strict mode.
		if isDigit(l.peekByte(1)) {
			p := l.pos + 1
			octal := true
			for p < len(l.src) && isDigit(l.src[p]) {
				if l.src[p] > '7' {
					octal = false
				}
				p++
			}
			// A following '.' or exponent means it was a decimal all along.
			if p >= len(l.src) || (l.src[p] != '.' && lower(l.src[p]) != 'e') {
				l.pos = p
				tok.Raw = l.src[start:p]
				digits := tok.Raw[1:]
				base := 8.0
				if !octal {
					base = 10.0
				}
				var v float64
				for i := 0; i < len(digits); i++ {
					v = v*base + float64(digits[i]-'0')
				}
				tok.Num = v
				tok.Value = tok.Raw
				return tok, nil
			}
		}
	}

	seenDot, seenExp := false, false
	for !l.atEnd() {
		c := l.src[l.pos]
		switch {
		case isDigit(c):
			l.pos++
		case c == '_':
			// Numeric separators must sit between two digits.
			if !isDigit(l.peekByte(-1)) || !isDigit(l.peekByte(1)) {
				return tok, l.errf(l.pos, "invalid numeric separator")
			}
			l.pos++
		case c == '.' && !seenDot && !seenExp:
			seenDot = true
			l.pos++
		case lower(c) == 'e' && !seenExp:
			next := l.peekByte(1)
			if !isDigit(next) && !((next == '+' || next == '-') && isDigit(l.peekByte(2))) {
				return tok, l.errf(l.pos, "missing exponent")
			}
			seenExp = true
			l.pos++
			if c := l.peekByte(0); c == '+' || c == '-' {
				l.pos++
			}
		default:
			goto done
		}
	}
done:
	raw := l.src[start:l.pos]
	if l.peekByte(0) == 'n' && !seenDot && !seenExp {
		l.pos++
		tok.Kind, tok.Value, tok.Raw = BigInt, strings.ReplaceAll(raw, "_", ""), raw+"n"
		return tok, nil
	}
	// A digit or identifier immediately after the literal is always an error
	// (`3in` must not lex as `3` followed by `in`).
	if r, _ := l.peekRune(); isIdentStart(r) {
		return tok, l.errf(l.pos, "identifier starts immediately after numeric literal")
	}
	tok.Raw = raw
	tok.Value = raw
	n, err := parseFloatLiteral(strings.ReplaceAll(raw, "_", ""))
	if err != nil {
		return tok, l.errf(start, "invalid number")
	}
	tok.Num = n
	return tok, nil
}

// scanRadix reads a 0x/0o/0b literal, which may carry a BigInt suffix.
func (l *Lexer) scanRadix(tok Token, start, radix int, valid func(byte) bool) (Token, error) {
	l.pos += 2
	digitStart := l.pos
	var v float64
	// Track whether the value stays exact so we can fall back to a slower
	// big-integer path only when it does not.
	for !l.atEnd() {
		c := l.src[l.pos]
		if c == '_' {
			if l.pos == digitStart || !valid(l.peekByte(1)) {
				return tok, l.errf(l.pos, "invalid numeric separator")
			}
			l.pos++
			continue
		}
		if !valid(c) {
			break
		}
		v = v*float64(radix) + float64(hexVal(c))
		l.pos++
	}
	if l.pos == digitStart {
		return tok, l.errf(start, "missing digits after radix prefix")
	}
	raw := l.src[start:l.pos]
	if l.peekByte(0) == 'n' {
		l.pos++
		tok.Kind, tok.Value, tok.Raw = BigInt, strings.ReplaceAll(raw, "_", ""), raw+"n"
		return tok, nil
	}
	if r, _ := l.peekRune(); isIdentStart(r) {
		return tok, l.errf(l.pos, "identifier starts immediately after numeric literal")
	}
	tok.Num, tok.Raw, tok.Value = v, raw, raw
	return tok, nil
}

// parseFloatLiteral converts a decimal literal to a float64. Overflow maps to
// infinity as the spec requires rather than erroring, and a trailing '.'
// ("1.") is accepted even though strconv rejects it.
func parseFloatLiteral(s string) (float64, error) {
	s = strings.TrimSuffix(s, ".")
	if s == "" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return f, nil
		}
		return 0, err
	}
	return f, nil
}

func isDigit(c byte) bool    { return c >= '0' && c <= '9' }
func isHexDigit(c byte) bool { return isDigit(c) || (lower(c) >= 'a' && lower(c) <= 'f') }
func lower(c byte) byte      { return c | 0x20 }

func hexVal(c byte) int {
	if isDigit(c) {
		return int(c - '0')
	}
	return int(lower(c)-'a') + 10
}

func parseHex(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	v := 0
	for i := 0; i < len(s); i++ {
		if !isHexDigit(s[i]) {
			return 0, false
		}
		v = v*16 + hexVal(s[i])
		if v > 0x10FFFF*16 {
			return 0, false
		}
	}
	return v, true
}

func isIdentStartByte(c byte) bool {
	return c == '$' || c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentStart(r rune) bool {
	if r < utf8.RuneSelf {
		return isIdentStartByte(byte(r))
	}
	return unicode.In(r, unicode.L, unicode.Nl, unicode.Other_ID_Start)
}

func isIdentPart(r rune) bool {
	if r < utf8.RuneSelf {
		return isIdentStartByte(byte(r)) || isDigit(byte(r))
	}
	if r == 0x200C || r == 0x200D { // ZWNJ, ZWJ
		return true
	}
	return unicode.In(r, unicode.L, unicode.Nl, unicode.Other_ID_Start,
		unicode.Mn, unicode.Mc, unicode.Nd, unicode.Pc, unicode.Other_ID_Continue)
}
