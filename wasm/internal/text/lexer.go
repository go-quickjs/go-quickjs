// Package text compiles the WebAssembly text format to the binary format. It
// holds the lexer and the module compiler that both the public wasm/wat
// package and the .wast script parser are built on.
//
// The grammar, its abbreviations and the order in which implicit types are
// added follow the reference interpreter's text parser
// (interpreter/text/lexer.mll and parser.mly in WebAssembly/spec).
package text

import (
	"fmt"
	"strconv"
	"unicode/utf8"
)

// Kind classifies a Token.
type Kind uint8

// The token kinds.
const (
	EOF Kind = iota
	LParen
	RParen
	Keyword // a keyword, including offset=N and align=N
	ID      // a symbolic identifier; Text is the name without the '$'
	String  // a string literal; Text holds its decoded bytes
	Nat     // an unsigned integer literal
	Int     // a signed integer literal
	Float   // a floating-point literal, including inf, nan and nan:0x...
	Reserved
)

// Token is a lexical token. Off is the byte offset of its first byte in the
// source, End the offset just past it; Line and Col are 1-based.
type Token struct {
	Kind Kind
	Text string
	Off  int
	End  int
	Line int
	Col  int
}

// Annot is an annotation, (@name item*). Items are its tokens, nested
// parentheses included, without the opening "(@name" and the closing ")".
// At is the index of the token that follows it in the token stream.
type Annot struct {
	Name  string
	Items []Token
	At    int
	Off   int
	Line  int
	Col   int
}

// Error is a text-format error at a source position.
type Error struct {
	Line int
	Col  int
	Msg  string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%d:%d: %s", e.Line, e.Col, e.Msg)
}

type lexer struct {
	src    []byte
	i      int
	line   int
	bol    int // offset of the current line's first byte
	toks   []Token
	annots []Annot
}

// Lex splits src into tokens and annotations. Comments and white space are
// dropped; annotations are taken out of the token stream.
func Lex(src []byte) ([]Token, []Annot, error) {
	lx := &lexer{src: src, line: 1}
	if err := lx.run(); err != nil {
		return nil, nil, err
	}
	return lx.toks, lx.annots, nil
}

func (lx *lexer) errAt(off int, format string, args ...any) error {
	line, col := lx.position(off)
	return &Error{Line: line, Col: col, Msg: fmt.Sprintf(format, args...)}
}

// position returns the line and column of offset off, which is at or after
// the start of the current line, or anywhere before it.
func (lx *lexer) position(off int) (int, int) {
	if off >= lx.bol {
		return lx.line, off - lx.bol + 1
	}
	line, bol := 1, 0
	for j := 0; j < off; j++ {
		if lx.src[j] == '\n' {
			line++
			bol = j + 1
		}
	}
	return line, off - bol + 1
}

func (lx *lexer) newline(at int) {
	lx.line++
	lx.bol = at + 1
}

func isIDChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	switch c {
	case '_', '+', '-', '*', '/', '\\', '^', '~', '=', '<', '>', '!', '?', '@', '#', '$', '%', '&', '|', ':', '`', '.', '\'':
		return true
	}
	return false
}

func (lx *lexer) run() error {
	for {
		tok, annot, err := lx.next(false)
		if err != nil {
			return err
		}
		if annot != nil {
			annot.At = len(lx.toks)
			lx.annots = append(lx.annots, *annot)
			continue
		}
		lx.toks = append(lx.toks, tok)
		if tok.Kind == EOF {
			return nil
		}
	}
}

// skipSpace skips white space and comments. inAnnot selects the error an
// unexpected character in a line comment reports.
func (lx *lexer) skipSpace() error {
	src := lx.src
	for lx.i < len(src) {
		c := src[lx.i]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			lx.i++
		case c == '\n':
			lx.newline(lx.i)
			lx.i++
		case c == ';' && lx.i+1 < len(src) && src[lx.i+1] == ';':
			lx.i += 2
			for lx.i < len(src) && src[lx.i] != '\n' && src[lx.i] != '\r' {
				if src[lx.i] < 0x80 {
					lx.i++
					continue
				}
				r, n := utf8.DecodeRune(src[lx.i:])
				if r == utf8.RuneError && n <= 1 {
					return lx.errAt(lx.i, "malformed UTF-8 encoding")
				}
				lx.i += n
			}
		case c == '(' && lx.i+1 < len(src) && src[lx.i+1] == ';':
			if err := lx.blockComment(); err != nil {
				return err
			}
		default:
			return nil
		}
	}
	return nil
}

func (lx *lexer) blockComment() error {
	src := lx.src
	start := lx.i
	lx.i += 2
	depth := 1
	for lx.i < len(src) {
		c := src[lx.i]
		switch {
		case c == ';' && lx.i+1 < len(src) && src[lx.i+1] == ')':
			lx.i += 2
			depth--
			if depth == 0 {
				return nil
			}
		case c == '(' && lx.i+1 < len(src) && src[lx.i+1] == ';':
			lx.i += 2
			depth++
		case c == '\n':
			lx.newline(lx.i)
			lx.i++
		case c < 0x80:
			lx.i++
		default:
			r, n := utf8.DecodeRune(src[lx.i:])
			if r == utf8.RuneError && n <= 1 {
				return lx.errAt(lx.i, "malformed UTF-8 encoding")
			}
			lx.i += n
		}
	}
	return lx.errAt(start, "unclosed comment")
}

// next returns the next token, or the next annotation, which it reads whole.
// In an annotation (inAnnot) reserved words are tokens, and characters that
// start no token are reported as illegal.
func (lx *lexer) next(inAnnot bool) (Token, *Annot, error) {
	if err := lx.skipSpace(); err != nil {
		return Token{}, nil, err
	}
	src := lx.src
	start := lx.i
	mk := func(k Kind, text string) Token {
		return Token{Kind: k, Text: text, Off: start, End: lx.i, Line: lx.line, Col: start - lx.bol + 1}
	}
	if lx.i >= len(src) {
		return mk(EOF, ""), nil, nil
	}
	c := src[lx.i]
	switch {
	case c == '(':
		if lx.i+1 < len(src) && src[lx.i+1] == '@' &&
			(!inAnnot || lx.i+2 < len(src) && (src[lx.i+2] == '"' || isIDChar(src[lx.i+2]))) {
			// In an annotation, "(@" without a name is a parenthesis.
			a, err := lx.annotation()
			return Token{}, a, err
		}
		lx.i++
		return mk(LParen, "("), nil, nil
	case c == ')':
		lx.i++
		return mk(RParen, ")"), nil, nil
	case c == '"' || isIDChar(c):
		tok, err := lx.word(inAnnot)
		return tok, nil, err
	case c == ',' || c == ';' || c == '[' || c == ']' || c == '{' || c == '}':
		lx.i++
		if inAnnot {
			return mk(Reserved, string(c)), nil, nil
		}
		return Token{}, nil, lx.errAt(start, "unknown operator %s", string(c))
	}
	if inAnnot {
		if c >= 0x80 {
			if r, n := utf8.DecodeRune(src[lx.i:]); r == utf8.RuneError && n <= 1 {
				return Token{}, nil, lx.errAt(start, "malformed UTF-8 encoding")
			}
		}
		return Token{}, nil, lx.errAt(start, "illegal character")
	}
	if c < 0x20 || c == 0x7f {
		return Token{}, nil, lx.errAt(start, "misplaced control character")
	}
	if r, n := utf8.DecodeRune(src[lx.i:]); r == utf8.RuneError && n <= 1 {
		return Token{}, nil, lx.errAt(start, "malformed UTF-8 encoding")
	}
	return Token{}, nil, lx.errAt(start, "misplaced unicode character")
}

// word reads a run of identifier characters and strings and classifies it.
func (lx *lexer) word(inAnnot bool) (Token, error) {
	src := lx.src
	start := lx.i
	line, col := lx.line, start-lx.bol+1
	strings := 0
	var firstStr string
	firstStrEnd := -1
	for lx.i < len(src) {
		c := src[lx.i]
		if isIDChar(c) {
			lx.i++
			continue
		}
		if c != '"' {
			break
		}
		s, err := lx.str()
		if err != nil {
			return Token{}, err
		}
		if strings == 0 {
			firstStr = s
			firstStrEnd = lx.i
		}
		strings++
	}
	text := string(src[start:lx.i])
	tok := Token{Text: text, Off: start, End: lx.i, Line: line, Col: col}
	switch {
	case strings == 1 && src[start] == '"' && firstStrEnd == lx.i:
		tok.Kind, tok.Text = String, firstStr
		return tok, nil
	case strings == 1 && src[start] == '$' && start+1 < len(src) && src[start+1] == '"' && firstStrEnd == lx.i:
		if firstStr == "" {
			return Token{}, lx.errAt(start, "empty identifier")
		}
		if !utf8.ValidString(firstStr) {
			return Token{}, lx.errAt(start, "malformed UTF-8 encoding")
		}
		tok.Kind, tok.Text = ID, firstStr
		return tok, nil
	case strings > 0:
		if inAnnot {
			tok.Kind = Reserved
			return tok, nil
		}
		return Token{}, lx.errAt(start, "unknown operator %s", text)
	}
	if text[0] == '$' {
		if len(text) == 1 {
			return Token{}, lx.errAt(start, "empty identifier")
		}
		tok.Kind, tok.Text = ID, text[1:]
		return tok, nil
	}
	if k := numKind(text); k != EOF {
		tok.Kind = k
		return tok, nil
	}
	if isKeyword(text) {
		tok.Kind = Keyword
		return tok, nil
	}
	for _, p := range [...]string{"offset=", "align="} {
		if len(text) > len(p) && text[:len(p)] == p && isNat(text[len(p):]) {
			tok.Kind = Keyword
			return tok, nil
		}
	}
	if inAnnot {
		tok.Kind = Reserved
		return tok, nil
	}
	return Token{}, lx.errAt(start, "unknown operator %s", text)
}

func isKeyword(s string) bool {
	if len(s) < 2 || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == ':') {
			return false
		}
	}
	return true
}

// str reads the string literal at lx.i and returns its decoded bytes.
func (lx *lexer) str() (string, error) {
	src := lx.src
	start := lx.i
	lx.i++
	var b []byte
	for {
		if lx.i >= len(src) {
			return "", lx.errAt(start, "unclosed string literal")
		}
		c := src[lx.i]
		switch {
		case c == '"':
			lx.i++
			return string(b), nil
		case c == '\n' || c == '\r':
			return "", lx.errAt(start, "unclosed string literal")
		case c < 0x20 || c == 0x7f:
			return "", lx.errAt(lx.i, "illegal control character in string literal")
		case c == '\\':
			esc := lx.i
			lx.i++
			if lx.i >= len(src) {
				return "", lx.errAt(start, "unclosed string literal")
			}
			e := src[lx.i]
			switch e {
			case 'n':
				b = append(b, '\n')
				lx.i++
			case 'r':
				b = append(b, '\r')
				lx.i++
			case 't':
				b = append(b, '\t')
				lx.i++
			case '\\', '\'', '"':
				b = append(b, e)
				lx.i++
			case 'u':
				j := lx.i + 1
				if j >= len(src) || src[j] != '{' {
					return "", lx.errAt(esc, "illegal escape")
				}
				k := j + 1
				for k < len(src) && (isHex(src[k]) || src[k] == '_') {
					k++
				}
				if k >= len(src) || src[k] != '}' || !isHexNum(string(src[j+1:k])) {
					return "", lx.errAt(esc, "illegal escape")
				}
				v, err := strconv.ParseUint(stripUnderscores(string(src[j+1:k])), 16, 32)
				if err != nil || v >= 0xd800 && v < 0xe000 || v > 0x10ffff {
					return "", lx.errAt(esc, "malformed UTF-8 codepoint")
				}
				b = utf8.AppendRune(b, rune(v))
				lx.i = k + 1
			default:
				if lx.i+1 < len(src) && isHex(e) && isHex(src[lx.i+1]) {
					b = append(b, unhex(e)<<4|unhex(src[lx.i+1]))
					lx.i += 2
					continue
				}
				return "", lx.errAt(esc, "illegal escape")
			}
		case c < 0x80:
			b = append(b, c)
			lx.i++
		default:
			r, n := utf8.DecodeRune(src[lx.i:])
			if r == utf8.RuneError && n <= 1 {
				return "", lx.errAt(lx.i, "malformed UTF-8 encoding")
			}
			b = append(b, src[lx.i:lx.i+n]...)
			lx.i += n
		}
	}
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}

// annotation reads an annotation starting at "(@".
func (lx *lexer) annotation() (*Annot, error) {
	src := lx.src
	start := lx.i
	a := &Annot{Off: start, Line: lx.line, Col: start - lx.bol + 1}
	lx.i += 2
	switch {
	case lx.i < len(src) && src[lx.i] == '"':
		s, err := lx.str()
		if err != nil {
			return nil, err
		}
		if s == "" {
			return nil, lx.errAt(start, "empty annotation id")
		}
		if !utf8.ValidString(s) {
			return nil, lx.errAt(start, "malformed UTF-8 encoding")
		}
		a.Name = s
	case lx.i < len(src) && isIDChar(src[lx.i]):
		j := lx.i
		for lx.i < len(src) && isIDChar(src[lx.i]) {
			lx.i++
		}
		a.Name = string(src[j:lx.i])
	default:
		return nil, lx.errAt(start, "empty annotation id")
	}
	depth := 0
	for {
		tok, nested, err := lx.next(true)
		if err != nil {
			return nil, err
		}
		if nested != nil {
			// A nested annotation is kept as its parenthesized items.
			a.Items = append(a.Items, Token{Kind: LParen, Text: "(", Off: nested.Off, Line: nested.Line, Col: nested.Col})
			a.Items = append(a.Items, Token{Kind: Reserved, Text: "@" + nested.Name, Off: nested.Off, Line: nested.Line, Col: nested.Col})
			a.Items = append(a.Items, nested.Items...)
			a.Items = append(a.Items, Token{Kind: RParen, Text: ")", Off: nested.Off, Line: nested.Line, Col: nested.Col})
			continue
		}
		switch tok.Kind {
		case EOF:
			return nil, lx.errAt(start, "unclosed annotation")
		case LParen:
			depth++
		case RParen:
			if depth == 0 {
				return a, nil
			}
			depth--
		}
		a.Items = append(a.Items, tok)
	}
}

// numKind classifies s as a Nat, Int or Float literal, or returns EOF.
func numKind(s string) Kind {
	if isNat(s) {
		return Nat
	}
	if (s[0] == '+' || s[0] == '-') && isNat(s[1:]) {
		return Int
	}
	if isFloat(s) {
		return Float
	}
	return EOF
}

func isNum(s string) bool { return isDigits(s, false) }

func isHexNum(s string) bool { return isDigits(s, true) }

// isDigits reports whether s is a non-empty run of digits with single
// underscores between them.
func isDigits(s string, hex bool) bool {
	if s == "" {
		return false
	}
	prevDigit := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '_' {
			if !prevDigit {
				return false
			}
			prevDigit = false
			continue
		}
		if !(c >= '0' && c <= '9' || hex && isHex(c)) {
			return false
		}
		prevDigit = true
	}
	return prevDigit
}

func isNat(s string) bool {
	if len(s) > 2 && s[0] == '0' && s[1] == 'x' {
		return isHexNum(s[2:])
	}
	return isNum(s)
}

func isFloat(s string) bool {
	if s != "" && (s[0] == '+' || s[0] == '-') {
		s = s[1:]
	}
	switch s {
	case "inf", "nan":
		return true
	}
	if len(s) > 6 && s[:6] == "nan:0x" {
		return isHexNum(s[6:])
	}
	hex := len(s) > 2 && s[0] == '0' && s[1] == 'x'
	expChars := "eE"
	if hex {
		s = s[2:]
		expChars = "pP"
	}
	// mantissa: digits ('.' digits?)?
	i := 0
	for i < len(s) && (s[i] == '_' || s[i] >= '0' && s[i] <= '9' || hex && isHex(s[i])) {
		i++
	}
	if !isDigits(s[:i], hex) {
		return false
	}
	dot, exp := false, false
	if i < len(s) && s[i] == '.' {
		dot = true
		i++
		j := i
		for i < len(s) && (s[i] == '_' || s[i] >= '0' && s[i] <= '9' || hex && isHex(s[i])) {
			i++
		}
		if i > j && !isDigits(s[j:i], hex) {
			return false
		}
	}
	if i < len(s) && (s[i] == expChars[0] || s[i] == expChars[1]) {
		exp = true
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		if !isNum(s[i:]) {
			return false
		}
		i = len(s)
	}
	return i == len(s) && (dot || exp)
}

func stripUnderscores(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '_' {
			b := make([]byte, 0, len(s))
			for j := 0; j < len(s); j++ {
				if s[j] != '_' {
					b = append(b, s[j])
				}
			}
			return string(b)
		}
	}
	return s
}
