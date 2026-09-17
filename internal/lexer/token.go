// Package lexer implements the ECMAScript tokenizer.
//
// The lexer is context-sensitive in two places, mirroring the grammar: whether
// a '/' starts a regular expression or is a division operator, and whether a
// '}' closes a block or resumes a template literal. Both are resolved by the
// parser telling the lexer which it expects, via ScanRegExp and ScanTemplateTail.
package lexer

import "fmt"

// Kind classifies a token.
type Kind uint8

const (
	EOF Kind = iota
	Ident
	PrivateIdent // #name
	Keyword
	Number
	BigInt
	String
	// The four template kinds distinguish where a part sits in the literal,
	// which the parser needs in order to know whether a substitution follows.
	Template     // `no substitution`
	TemplateHead // `a${
	TemplateMid  // }b${
	TemplateTail // }c`
	Regexp
	Punct
)

func (k Kind) String() string {
	switch k {
	case EOF:
		return "EOF"
	case Ident:
		return "identifier"
	case PrivateIdent:
		return "private identifier"
	case Keyword:
		return "keyword"
	case Number:
		return "number"
	case BigInt:
		return "bigint"
	case String:
		return "string"
	case Template, TemplateHead, TemplateMid, TemplateTail:
		return "template"
	case Regexp:
		return "regexp"
	case Punct:
		return "punctuator"
	}
	return "unknown"
}

// Token is a single lexical unit.
//
// Value carries the cooked (escape-processed) text for identifiers, strings and
// template parts, the raw source text for numbers and regexps, and the operator
// spelling for punctuators and keywords.
type Token struct {
	Kind  Kind
	Value string
	// Num holds the parsed value for Kind == Number.
	Num float64
	// Raw is the uninterpreted source text. Templates need it for String.raw,
	// and regexps need it to rebuild the literal.
	Raw string
	// Flags holds the trailing flags of a regexp literal.
	Flags string
	// TemplateValid reports whether a template part's escape sequences were all
	// well formed. An invalid escape is fatal for an untagged template but not
	// for a tagged one, where the cooked value simply becomes undefined, so the
	// lexer records it rather than failing.
	TemplateValid bool
	// LegacyEscape reports whether a string literal used one of the escape
	// sequences that predate strict mode: an octal escape, or \8 and \9. The
	// lexer records it rather than failing, because whether it is an error
	// depends on a strictness the lexer does not know.
	LegacyEscape bool
	// Escaped reports whether an identifier was written with a unicode escape.
	// Such a name is never a keyword -- \u0069f is an identifier -- and it is
	// never a contextual keyword either, so `\u0067et x() {}` is a method named
	// "get" rather than an accessor.
	Escaped bool
	// NewlineBefore reports whether a LineTerminator preceded this token. This
	// is what drives automatic semicolon insertion and the restricted
	// productions (return/throw/break/continue, postfix ++/--).
	NewlineBefore bool
	Pos           int // byte offset of the token start
	Line          int // 1-based
	// LineStart is the byte offset of the first byte of Line. Columns are
	// derived from it on demand rather than measured for every token, because
	// counting runes eagerly makes scanning a long line quadratic.
	LineStart int
}

// Is reports whether the token is of kind k with the given text.
func (t Token) Is(k Kind, v string) bool { return t.Kind == k && t.Value == v }

// IsPunct reports whether the token is the given punctuator.
func (t Token) IsPunct(v string) bool { return t.Kind == Punct && t.Value == v }

// IsKeyword reports whether the token is the given keyword.
func (t Token) IsKeyword(v string) bool { return t.Kind == Keyword && t.Value == v }

func (t Token) String() string {
	switch t.Kind {
	case EOF:
		return "end of input"
	case String:
		return fmt.Sprintf("string %q", t.Value)
	case Number:
		return "number " + t.Raw
	default:
		if t.Value == "" {
			return t.Kind.String()
		}
		return fmt.Sprintf("%s %q", t.Kind, t.Value)
	}
}

// reservedWords are the ECMAScript reserved words. Contextual keywords (async,
// let, static, get, set, of, ...) are deliberately absent: they lex as
// identifiers and the parser decides what they mean from position.
//
// "yield" and "await" are contextual too. Each is an operator only inside a
// generator or async function respectively, and an ordinary identifier
// everywhere else, so `var yield = 1` is legal sloppy-mode code.
var reservedWords = map[string]bool{
	"break": true, "case": true, "catch": true, "class": true,
	"const": true, "continue": true, "debugger": true, "default": true,
	"delete": true, "do": true, "else": true, "enum": true, "export": true,
	"extends": true, "false": true, "finally": true, "for": true,
	"function": true, "if": true, "import": true, "in": true,
	"instanceof": true, "new": true, "null": true, "return": true,
	"super": true, "switch": true, "this": true, "throw": true, "true": true,
	"try": true, "typeof": true, "var": true, "void": true, "while": true,
	"with": true,
}

// IsReservedWord reports whether name is a reserved word and so cannot be used
// as a binding identifier.
func IsReservedWord(name string) bool { return reservedWords[name] }

// punctuators, longest first within each starting byte, so that the scanner can
// greedily match the longest operator (>>>= before >>> before >> before >).
var punctuators = []string{
	">>>=", "...", "===", "!==", "**=", "<<=", ">>=", ">>>", "&&=", "||=", "??=",
	"=>", "==", "!=", "<=", ">=", "&&", "||", "??", "?.", "++", "--", "+=", "-=",
	"*=", "/=", "%=", "&=", "|=", "^=", "<<", ">>", "**",
	"{", "}", "(", ")", "[", "]", ";", ",", "<", ">", "+", "-", "*", "/", "%",
	"&", "|", "^", "!", "~", "?", ":", "=", ".", "#", "@",
}
