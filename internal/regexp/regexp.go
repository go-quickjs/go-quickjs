package regexp

import "github.com/go-quickjs/go-quickjs/internal/wtf8"

// Regexp is a compiled pattern.
type Regexp struct {
	source     string
	flags      Flags
	prog       *program
	groupCount int
	groupNames map[string]int
}

// Compile parses and compiles a pattern.
func Compile(source, flags string) (*Regexp, error) {
	f, err := ParseFlags(flags)
	if err != nil {
		return nil, &SyntaxError{Msg: err.Error(), Pattern: source}
	}
	return CompileFlags(source, f)
}

// CompileFlags compiles a pattern with already-parsed flags.
func CompileFlags(source string, f Flags) (*Regexp, error) {
	tree, groups, names, err := parse(source, f)
	if err != nil {
		return nil, err
	}
	return &Regexp{
		source:     source,
		flags:      f,
		prog:       compileNode(tree, f, groups),
		groupCount: groups,
		groupNames: names,
	}, nil
}

// Source returns the pattern text.
func (re *Regexp) Source() string { return re.source }

// Flags returns the compiled flags.
func (re *Regexp) Flags() Flags { return re.flags }

// GroupCount returns the number of capturing groups.
func (re *Regexp) GroupCount() int { return re.groupCount }

// GroupNames maps each named group to its index.
func (re *Regexp) GroupNames() map[string]int { return re.groupNames }

// Match runs the pattern against a subject given as UTF-16 code units,
// beginning at start.
//
// The result holds 2*(groups+1) indices: the whole match's bounds followed by
// each group's, with -1 for a group that did not participate. A nil result
// means no match.
func (re *Regexp) Match(units []uint16, start int) ([]int, error) {
	if start < 0 {
		start = 0
	}
	if start > len(units) {
		return nil, nil
	}
	in := &input{units: units, unicode: re.flags&FlagUnicode != 0}
	return re.exec(in, start)
}

// MatchString is Match on a Go string, which is converted to code units first.
//
// The conversion preserves lone surrogates, so a pattern can match one.
func (re *Regexp) MatchString(s string, start int) ([]int, error) {
	return re.Match(wtf8.ToUTF16(s), start)
}

// QuoteMeta escapes the characters that have special meaning in a pattern, so
// that the result matches the literal text.
//
// String.prototype.replace and split accept a plain string where a pattern is
// expected, and must treat it literally.
func QuoteMeta(s string) string {
	var sb []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x80 && isMetaChar(c) {
			sb = append(sb, '\\')
		}
		sb = append(sb, c)
	}
	return string(sb)
}

func isMetaChar(c byte) bool {
	switch c {
	case '\\', '.', '+', '*', '?', '(', ')', '|', '[', ']', '{', '}', '^', '$', '/':
		return true
	}
	return false
}
