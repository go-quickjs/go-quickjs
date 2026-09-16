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
	Template    // `no substitution` or the head `a${
	TemplateMid // }b${
	TemplateTail
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
	case Template, TemplateMid, TemplateTail:
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
	// NewlineBefore reports whether a LineTerminator preceded this token. This
	// is what drives automatic semicolon insertion and the restricted
	// productions (return/throw/break/continue, postfix ++/--).
	NewlineBefore bool
	Pos           int // byte offset of the token start
	Line          int // 1-based
	Col           int // 1-based, in runes
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
var reservedWords = map[string]bool{
	"await": true, "break": true, "case": true, "catch": true, "class": true,
	"const": true, "continue": true, "debugger": true, "default": true,
	"delete": true, "do": true, "else": true, "enum": true, "export": true,
	"extends": true, "false": true, "finally": true, "for": true,
	"function": true, "if": true, "import": true, "in": true,
	"instanceof": true, "new": true, "null": true, "return": true,
	"super": true, "switch": true, "this": true, "throw": true, "true": true,
	"try": true, "typeof": true, "var": true, "void": true, "while": true,
	"with": true, "yield": true,
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
