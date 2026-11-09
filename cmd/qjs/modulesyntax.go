package main

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// looksLikeModule reports whether source uses the syntax only a module may:
// an import or an export declaration, or import.meta. As node detects a
// module, source that parses as a script is CommonJS, so what only looks like
// that syntax -- a line of a string, a template, a comment or a regular
// expression, a property or a method named import, an import() call -- does
// not count.
//
// It reads the source as tokens, not as a program: only enough to know where a
// string, a template, a comment and a regular expression begin and end, and
// how deep in brackets each token is, because a declaration can only be at the
// top. A slash is a division or a regular expression by the token before it,
// as tokenizers that do not parse decide it; the cases that guess wrong --
// `a = {} / 2`, or a regular expression after `)` that is not an if's, a
// while's, a for's or a with's -- are not code anyone writes. A top-level
// await, which node takes for a module's too, is not looked for: telling it
// from a name await needs the parser.
func looksLikeModule(src string) bool {
	if !hasWord(src, "import") && !hasWord(src, "export") {
		return false
	}
	// The brackets open where the scanner is: '(', '[' and '{', 'c' for the
	// parenthesis after if, while, for or with, after which a slash starts a
	// regular expression, and '$' for a template's substitution, whose '}'
	// goes back into the template.
	var buf [32]byte
	stack := buf[:0]
	i := 0
	if strings.HasPrefix(src, "\uFEFF") {
		i = len("\uFEFF")
	}
	if strings.HasPrefix(src[i:], "#!") {
		i = lineEnd(src, i)
	}
	// regexOK says a slash here starts a regular expression; dot that the
	// last token was a dot, after which a name is a property's; control that
	// it was if, while, for or with.
	regexOK, dot, control := true, false, false
	lineStart := true
	for {
		// Spaces and line ends here, the rest of what is not a token in
		// skipTrivia.
		for ; i < len(src); i++ {
			if c := src[i]; c == '\n' || c == '\r' {
				lineStart = true
			} else if c != ' ' && c != '\t' {
				break
			}
		}
		if i >= len(src) {
			return false
		}
		if c := src[i]; c == '/' || c == '<' || c == '-' || c == '\v' || c == '\f' || c >= utf8.RuneSelf {
			if i, lineStart = skipTrivia(src, i, lineStart); i >= len(src) {
				return false
			}
		}
		lineStart = false
		afterDot, afterControl := dot, control
		dot, control = false, false
		c := src[i]
		switch {
		case nameStart[c]:
			start := i
			i = nameEnd(src, i)
			word := src[start:i]
			if afterDot {
				regexOK = false
				continue
			}
			switch word {
			case "export":
				if len(stack) == 0 {
					return true
				}
			case "import":
				j, _ := skipTrivia(src, i, false)
				switch byteAt(src, j) {
				case '(':
					// import(): a call, which a script may make.
				case '.':
					// import.meta, wherever it is, is a module's alone;
					// import.source() and import.defer() are calls.
					k, _ := skipTrivia(src, j+1, false)
					if strings.HasPrefix(src[k:], "meta") && nameEnd(src, k) == k+len("meta") {
						return true
					}
				default:
					// A declaration, at the top; deeper, a method or a
					// property named import.
					if len(stack) == 0 {
						return true
					}
				}
			}
			switch word {
			case "return", "typeof", "instanceof", "in", "of", "new", "delete", "void",
				"throw", "case", "do", "else", "yield", "await":
				regexOK = true
			case "if", "while", "for", "with":
				regexOK, control = true, true
			default:
				regexOK = false
			}
		case c >= '0' && c <= '9' || c == '.' && isDigit(byteAt(src, i+1)):
			i = numberEnd(src, i)
			regexOK = false
		case c == '"' || c == '\'':
			i = stringEnd(src, i)
			regexOK = false
		case c == '`':
			var closed bool
			i, closed = templateEnd(src, i+1)
			if !closed {
				stack = append(stack, '$')
			}
			regexOK = !closed
		case c == '/' && regexOK:
			i = regExpEnd(src, i)
			regexOK = false
		case c == '(':
			if afterControl {
				stack = append(stack, 'c')
			} else {
				stack = append(stack, '(')
			}
			i++
			regexOK = true
		case c == '[' || c == '{':
			stack = append(stack, c)
			i++
			regexOK = true
		case c == ')' || c == ']' || c == '}':
			var top byte
			if len(stack) > 0 {
				top = stack[len(stack)-1]
				stack = stack[:len(stack)-1]
			}
			if c == '}' && top == '$' {
				var closed bool
				i, closed = templateEnd(src, i+1)
				if !closed {
					stack = append(stack, '$')
				}
				regexOK = !closed
				continue
			}
			i++
			// After a block's '}' a slash starts a statement; after an
			// object's it would divide an object, which nobody does.
			regexOK = c == '}' || top == 'c'
		case c == '.':
			if strings.HasPrefix(src[i:], "...") {
				i += 3
				regexOK = true
			} else {
				i++
				dot = true
			}
		case (c == '+' || c == '-') && byteAt(src, i+1) == c:
			// a++ / 2 divides.
			i += 2
			regexOK = false
		default:
			i++
			regexOK = true
		}
	}
}

// hasWord says whether word is in src but not inside a longer name, as every
// import and export token is: what has neither, as CommonJS's require and
// module.exports do not, need not be scanned. A character past ASCII next to
// one may or may not be a name's, and is taken not to be.
func hasWord(src, word string) bool {
	for i := 0; ; {
		j := strings.Index(src[i:], word)
		if j < 0 {
			return false
		}
		j += i
		i = j + len(word)
		if (j == 0 || !namePart[src[j-1]] && src[j-1] != '\\') && (i == len(src) || !namePart[src[i]]) {
			return true
		}
	}
}

// skipTrivia skips whitespace and comments from i, a script's HTML-like
// comments among them, and says whether a line has started with nothing but
// them since, where --> starts a comment.
func skipTrivia(src string, i int, lineStart bool) (int, bool) {
	for i < len(src) {
		c := src[i]
		switch c {
		case ' ', '\t', '\v', '\f':
			i++
		case '\n', '\r':
			i++
			lineStart = true
		case '/':
			switch byteAt(src, i+1) {
			case '/':
				i = lineEnd(src, i)
			case '*':
				end := strings.Index(src[i+2:], "*/")
				if end < 0 {
					return len(src), lineStart
				}
				if strings.ContainsAny(src[i+2:i+2+end], "\n\r\u2028\u2029") {
					lineStart = true
				}
				i += 2 + end + 2
			default:
				return i, lineStart
			}
		case '<':
			if !strings.HasPrefix(src[i:], "<!--") {
				return i, lineStart
			}
			i = lineEnd(src, i)
		case '-':
			if !lineStart || !strings.HasPrefix(src[i:], "-->") {
				return i, lineStart
			}
			i = lineEnd(src, i)
		default:
			if c < utf8.RuneSelf {
				return i, lineStart
			}
			r, n := utf8.DecodeRuneInString(src[i:])
			switch {
			case r == 0x2028 || r == 0x2029:
				lineStart = true
			case r == 0xFEFF || unicode.Is(unicode.Zs, r):
			default:
				return i, lineStart
			}
			i += n
		}
	}
	return i, lineStart
}

// lineEnd is where the line i is on ends: its line terminator, or the end.
func lineEnd(src string, i int) int {
	for i < len(src) {
		switch c := src[i]; {
		case c == '\n' || c == '\r':
			return i
		case c >= utf8.RuneSelf:
			r, n := utf8.DecodeRuneInString(src[i:])
			if r == 0x2028 || r == 0x2029 {
				return i
			}
			i += n
		default:
			i++
		}
	}
	return i
}

// nameStart says which bytes a name starts with: a letter, $, _, an escape, or
// a character past ASCII, which nameEnd looks at. namePart says which ASCII
// bytes go on with one, but for an escape.
var nameStart, namePart = func() (start, part [256]bool) {
	for c := range 256 {
		letter := c|0x20 >= 'a' && c|0x20 <= 'z' || c == '$' || c == '_'
		start[c] = letter || c == '\\' || c >= utf8.RuneSelf
		part[c] = letter || c >= '0' && c <= '9'
	}
	return start, part
}()

// nameEnd is where the name at i ends. Past ASCII, only what skipTrivia skips
// ends one: the scanner need not tell a name from a stray character.
func nameEnd(src string, i int) int {
	for i < len(src) {
		c := src[i]
		switch {
		case namePart[c]:
			i++
		case c == '\\':
			// \uXXXX or \u{X...}.
			i++
			if strings.HasPrefix(src[i:], "u{") {
				if end := strings.IndexByte(src[i:], '}'); end >= 0 {
					i += end + 1
				}
			}
		case c >= utf8.RuneSelf:
			r, n := utf8.DecodeRuneInString(src[i:])
			if r == 0x2028 || r == 0x2029 || r == 0xFEFF || unicode.Is(unicode.Zs, r) {
				return i
			}
			i += n
		default:
			return i
		}
	}
	return i
}

// numberEnd is where the number at i ends: its digits, letters, separators
// and dots, and the sign of an exponent.
func numberEnd(src string, i int) int {
	for i < len(src) {
		c := src[i]
		switch {
		case c|0x20 >= 'a' && c|0x20 <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '.':
			i++
		case (c == '+' || c == '-') && src[i-1]|0x20 == 'e':
			i++
		default:
			return i
		}
	}
	return i
}

// stringEnd is where the string at i ends: past its closing quote, or at the
// line terminator that leaves it unterminated.
func stringEnd(src string, i int) int {
	quote := src[i]
	for i++; i < len(src); i++ {
		switch src[i] {
		case quote:
			return i + 1
		case '\\':
			i++
			if strings.HasPrefix(src[i:], "\r\n") {
				i++
			}
		case '\n', '\r':
			return i
		}
	}
	return i
}

// templateEnd scans a template's characters from i and says where they end:
// past the closing backquote, closed, or past the ${ of a substitution.
func templateEnd(src string, i int) (int, bool) {
	for ; i < len(src); i++ {
		switch src[i] {
		case '`':
			return i + 1, true
		case '\\':
			i++
		case '$':
			if byteAt(src, i+1) == '{' {
				return i + 2, false
			}
		}
	}
	return i, true
}

// regExpEnd is where the regular expression at i ends, past its flags. A
// slash in a class does not end it.
func regExpEnd(src string, i int) int {
	class := false
	for i++; i < len(src); i++ {
		switch src[i] {
		case '\\':
			i++
		case '[':
			class = true
		case ']':
			class = false
		case '/':
			if !class {
				return nameEnd(src, i+1)
			}
		case '\n', '\r':
			return i
		}
	}
	return i
}

func byteAt(src string, i int) byte {
	if i < len(src) {
		return src[i]
	}
	return 0
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
