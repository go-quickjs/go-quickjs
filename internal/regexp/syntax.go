// Package regexp implements JavaScript regular expressions.
//
// Go's standard regexp package cannot be used: it is an RE2 engine, which
// guarantees linear time by refusing the features JavaScript requires --
// backreferences, lookahead and lookbehind. A JavaScript engine needs a
// backtracking matcher, with the exponential worst case that implies. The step
// budget in exec.go bounds that so a pathological pattern fails rather than
// hanging the host.
//
// Matching operates on UTF-16 code units, which is what JavaScript strings are.
// Under the `u` flag the unit stream is decoded to code points instead, so that
// a surrogate pair is one character and a quantifier applies to the whole of it.
package regexp

import (
	"fmt"
	"strings"
)

// Flags is the set of flags a pattern was compiled with.
type Flags uint16

const (
	// FlagGlobal makes the match advance lastIndex, so repeated calls walk
	// through the string.
	FlagGlobal Flags = 1 << iota
	// FlagIgnoreCase folds case when comparing characters.
	FlagIgnoreCase
	// FlagMultiline makes ^ and $ match at line boundaries as well as at the
	// ends of the input.
	FlagMultiline
	// FlagDotAll makes . match a line terminator, which it otherwise does not.
	FlagDotAll
	// FlagUnicode switches matching to code points and enables \p escapes.
	FlagUnicode
	// FlagSticky anchors the match at lastIndex rather than searching forward.
	FlagSticky
	// FlagHasIndices makes exec report the bounds of each capture.
	FlagHasIndices
	// FlagUnicodeSets is the `v` flag, a superset of `u` with set notation in
	// character classes.
	FlagUnicodeSets
)

// ParseFlags converts the flag string of a literal into a Flags value.
func ParseFlags(s string) (Flags, error) {
	var f Flags
	seen := make(map[rune]bool, len(s))
	for _, c := range s {
		if seen[c] {
			return 0, fmt.Errorf("duplicate flag %q", c)
		}
		seen[c] = true
		switch c {
		case 'g':
			f |= FlagGlobal
		case 'i':
			f |= FlagIgnoreCase
		case 'm':
			f |= FlagMultiline
		case 's':
			f |= FlagDotAll
		case 'u':
			f |= FlagUnicode
		case 'y':
			f |= FlagSticky
		case 'd':
			f |= FlagHasIndices
		case 'v':
			f |= FlagUnicodeSets | FlagUnicode
		default:
			return 0, fmt.Errorf("invalid flag %q", c)
		}
	}
	if f&FlagUnicodeSets != 0 && s != "" && strings.ContainsRune(s, 'u') {
		return 0, fmt.Errorf("the u and v flags cannot be combined")
	}
	return f, nil
}

// String renders the flags in the canonical order the specification uses.
func (f Flags) String() string {
	var sb strings.Builder
	// The order is fixed: d, g, i, m, s, u, v, y.
	if f&FlagHasIndices != 0 {
		sb.WriteByte('d')
	}
	if f&FlagGlobal != 0 {
		sb.WriteByte('g')
	}
	if f&FlagIgnoreCase != 0 {
		sb.WriteByte('i')
	}
	if f&FlagMultiline != 0 {
		sb.WriteByte('m')
	}
	if f&FlagDotAll != 0 {
		sb.WriteByte('s')
	}
	if f&FlagUnicode != 0 && f&FlagUnicodeSets == 0 {
		sb.WriteByte('u')
	}
	if f&FlagUnicodeSets != 0 {
		sb.WriteByte('v')
	}
	if f&FlagSticky != 0 {
		sb.WriteByte('y')
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// Syntax tree
// ---------------------------------------------------------------------------

// node is one element of a parsed pattern.
type node interface{ isNode() }

// nodeEmpty matches the empty string, which an empty alternative produces.
type nodeEmpty struct{}

// nodeChar matches one specific code point.
type nodeChar struct{ r rune }

// nodeAny is `.`, which matches any character except a line terminator unless
// the dotAll flag is set.
type nodeAny struct{ dotAll bool }

// nodeClass is a character class, either a bracketed set or a shorthand escape.
type nodeClass struct {
	set *charSet
}

// nodeSeq matches its elements in order.
type nodeSeq struct{ items []node }

// nodeAlt matches whichever alternative succeeds first, in source order.
type nodeAlt struct{ alts []node }

// nodeRepeat applies a quantifier.
type nodeRepeat struct {
	item     node
	min, max int // max is -1 for unbounded
	// greedy selects whether the longest or the shortest match is tried first.
	greedy bool
}

// nodeGroup is a parenthesized subexpression.
type nodeGroup struct {
	item node
	// index is the capture number, or 0 for a non-capturing group.
	index int
	name  string
}

// nodeAssert is a zero-width assertion.
//
// multiline is recorded per assertion rather than read from the pattern's flags
// at match time, because an inline modifier can turn m on or off for part of a
// pattern and the two halves must disagree.
type nodeAssert struct {
	kind      assertKind
	multiline bool
}

// nodeModifier is a group that changes i, m or s for what it contains:
// `(?i:...)` turns one on, `(?-i:...)` turns it off, `(?i-m:...)` does both.
//
// It exists so that a pattern can be case-insensitive in one place without
// being so everywhere, which otherwise takes two patterns or a hand-expanded
// character class. The flags it carries are the ones in effect inside it,
// already resolved against the enclosing ones.
type nodeModifier struct {
	flags Flags
	item  node
}

type assertKind uint8

const (
	assertStart assertKind = iota
	assertEnd
	assertWordBoundary
	assertNotWordBoundary
)

// nodeLook is a lookahead or lookbehind.
type nodeLook struct {
	item   node
	behind bool
	negate bool
}

// nodeBackref matches the text a previous group captured.
//
// It is referred to by pointer because a named reference may precede the group
// it names: the parser records the node and fills in the index once every name
// is known, so the tree has to hold the node itself rather than a copy.
type nodeBackref struct {
	index int
	name  string
}

func (nodeEmpty) isNode()    {}
func (nodeChar) isNode()     {}
func (nodeAny) isNode()      {}
func (nodeClass) isNode()    {}
func (nodeSeq) isNode()      {}
func (nodeAlt) isNode()      {}
func (nodeRepeat) isNode()   {}
func (nodeGroup) isNode()    {}
func (nodeAssert) isNode()   {}
func (nodeModifier) isNode() {}
func (nodeLook) isNode()     {}
func (*nodeBackref) isNode() {}
