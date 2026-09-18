// Package jsregexp implements ECMAScript regular expressions for Go.
//
// Go's own regexp package is RE2: it guarantees linear time by refusing the
// features that need backtracking. JavaScript requires exactly those features,
// so a pattern written for JavaScript often cannot be used with it at all:
//
//	(\w+)\s+\1          // a backreference
//	(?<=\$)\d+          // a lookbehind
//	(?<year>\d{4})-\d\d // a named group
//	\p{Script=Greek}+   // a Unicode property escape
//
// This package is the regular expression engine of a JavaScript engine, made
// usable on its own. It accepts the patterns and the flags a JavaScript
// program would write, matches by UTF-16 code unit as JavaScript does, and
// reports positions as byte offsets, which is what Go code expects.
//
//	re := jsregexp.MustCompile(`(?<user>\w+)@(\w+)\.com`, "i")
//	m, err := re.FindStringSubmatch("Write to Someone@Example.com today")
//	// m = ["Someone@Example.com", "Someone", "Example"]
//
// # Backtracking
//
// Backtracking buys those features at the price of an exponential worst case:
// a pattern like /(a+)+b/ against a long run of "a" takes practically for
// ever. A matcher that cannot be stopped is not safe to run on input a program
// did not write itself, so every match is bounded by a step budget, and a
// match that exhausts it reports ErrComplexity rather than running on. That is
// why the methods here return an error where Go's regexp does not: the budget
// is the one thing that can go wrong at match time.
//
// # Text
//
// JavaScript strings are sequences of UTF-16 code units, and the semantics of
// a pattern are defined over them: without the u flag, /./ matches one code
// unit, so it matches half of an emoji. Subjects are given and returned as Go
// strings, and positions are byte offsets into them, so that the results index
// the string that was passed in. A lone surrogate survives the round trip: it
// is encoded as WTF-8, which is UTF-8 for every string that has none.
package jsregexp

import (
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	re "github.com/go-quickjs/go-quickjs/internal/regexp"
	"github.com/go-quickjs/go-quickjs/internal/wtf8"
)

// ErrComplexity reports that a match exhausted its step budget, which a pattern
// that backtracks catastrophically does. It is the only error a match reports.
var ErrComplexity = re.ErrComplexity

// Regexp is a compiled pattern.
//
// A Regexp is safe for concurrent use by multiple goroutines only through the
// methods that take a subject: compiling one and sharing it is fine, and every
// match allocates the state it needs.
type Regexp struct {
	re    *re.Regexp
	src   string
	flags string
	names []string
}

// Compile parses a pattern and its flags.
//
// The flags are the JavaScript ones, in any order and at most one of each:
//
//	d  record the position of every group
//	g  find every match rather than the first (see FindAll)
//	i  match without regard to case
//	m  ^ and $ match at a line break as well as at the ends
//	s  . matches a line break too
//	u  interpret the pattern as Unicode code points
//	v  as u, with set notation and properties of strings
//	y  match only at the position the search starts from
//
// The g flag has little to say here: which matches are found is decided by the
// method called rather than by the pattern. The y flag does: a sticky pattern
// matches at the position a search starts from or not at all.
func Compile(pattern, flags string) (*Regexp, error) {
	compiled, err := re.Compile(pattern, flags)
	if err != nil {
		return nil, err
	}
	return newRegexp(compiled, pattern, flags), nil
}

// MustCompile is Compile for a pattern known to be good, such as a literal in
// the program. It panics if the pattern does not compile.
func MustCompile(pattern, flags string) *Regexp {
	r, err := Compile(pattern, flags)
	if err != nil {
		panic("jsregexp: " + err.Error())
	}
	return r
}

func newRegexp(compiled *re.Regexp, pattern, flags string) *Regexp {
	// The names are held the way Go's regexp holds them: one entry per group,
	// empty where the group has no name, with the whole match at index zero.
	names := make([]string, compiled.GroupCount()+1)
	for name, i := range compiled.GroupNames() {
		if i >= 0 && i < len(names) {
			names[i] = name
		}
	}
	return &Regexp{re: compiled, src: pattern, flags: flags, names: names}
}

// String returns the pattern text, without delimiters or flags.
func (r *Regexp) String() string { return r.src }

// Flags returns the flags the pattern was compiled with.
func (r *Regexp) Flags() string { return r.flags }

// NumSubexp returns the number of capturing groups.
func (r *Regexp) NumSubexp() int { return r.re.GroupCount() }

// SubexpNames returns the name of each capturing group, in order, with an empty
// string where a group has no name. The first entry is always empty: it stands
// for the whole match, which is not a group.
func (r *Regexp) SubexpNames() []string { return r.names }

// SubexpIndex returns the index of the group with the given name, or -1.
func (r *Regexp) SubexpIndex(name string) int {
	if name == "" {
		return -1
	}
	if i, ok := r.re.GroupNames()[name]; ok {
		return i
	}
	return -1
}

// QuoteMeta returns a pattern that matches the literal text of s.
func QuoteMeta(s string) string { return re.QuoteMeta(s) }

// ---------------------------------------------------------------------------
// Matching
// ---------------------------------------------------------------------------

// subject is a string prepared for matching: its code units, and where each one
// begins in the original bytes.
//
// The offsets are what turns a match's positions back into indices into the
// string the caller passed in. A subject that is all ASCII needs none, since a
// code unit is then a byte.
type subject struct {
	s       string
	units   []uint16
	offsets []int
	ascii   bool
}

func newSubject(s string) *subject {
	if isASCII(s) {
		return &subject{s: s, units: asciiUnits(s), ascii: true}
	}
	units := make([]uint16, 0, len(s))
	offsets := make([]int, 0, len(s)+1)
	for i := 0; i < len(s); {
		r, size := wtf8.DecodeRune(s[i:])
		if r > 0xFFFF {
			hi, lo := utf16.EncodeRune(r)
			units = append(units, uint16(hi), uint16(lo))
			offsets = append(offsets, i, i)
		} else {
			units = append(units, uint16(r))
			offsets = append(offsets, i)
		}
		i += size
	}
	offsets = append(offsets, len(s))
	return &subject{s: s, units: units, offsets: offsets}
}

// byteAt returns the byte offset of a code-unit position.
func (sub *subject) byteAt(unit int) int {
	if unit < 0 {
		return -1
	}
	if sub.ascii {
		return unit
	}
	if unit >= len(sub.offsets) {
		return len(sub.s)
	}
	return sub.offsets[unit]
}

// text returns the subject between two code-unit positions.
//
// A slice that begins or ends inside a surrogate pair cannot be a slice of the
// original bytes -- the pair is one four-byte sequence there and two halves
// here -- so those are re-encoded.
func (sub *subject) text(lo, hi int) string {
	if lo < 0 || hi < 0 {
		return ""
	}
	if sub.ascii {
		return sub.s[lo:hi]
	}
	if sub.splits(lo) || sub.splits(hi) {
		return wtf8.FromUTF16(sub.units[lo:hi])
	}
	return sub.s[sub.byteAt(lo):sub.byteAt(hi)]
}

// splits reports whether a position falls between the halves of a pair.
func (sub *subject) splits(unit int) bool {
	return unit > 0 && unit < len(sub.offsets) && sub.offsets[unit] == sub.offsets[unit-1]
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

func asciiUnits(s string) []uint16 {
	units := make([]uint16, len(s))
	for i := 0; i < len(s); i++ {
		units[i] = uint16(s[i])
	}
	return units
}

// MatchString reports whether the pattern matches anywhere in s.
func (r *Regexp) MatchString(s string) (bool, error) {
	sub := newSubject(s)
	caps, err := r.re.Match(sub.units, 0)
	return caps != nil, err
}

// MatchString reports whether a pattern matches anywhere in s, compiling it
// first. Compiling once with Compile is better where the pattern is reused.
func MatchString(pattern, flags, s string) (bool, error) {
	r, err := Compile(pattern, flags)
	if err != nil {
		return false, err
	}
	return r.MatchString(s)
}

// FindString returns the leftmost match, or "" if there is none.
//
// An empty string is also what a pattern that matched emptiness returns, so
// FindStringIndex is what tells the two apart.
func (r *Regexp) FindString(s string) (string, error) {
	sub := newSubject(s)
	caps, err := r.re.Match(sub.units, 0)
	if err != nil || caps == nil {
		return "", err
	}
	return sub.text(caps[0], caps[1]), nil
}

// FindStringIndex returns the byte bounds of the leftmost match, or nil.
func (r *Regexp) FindStringIndex(s string) ([]int, error) {
	sub := newSubject(s)
	caps, err := r.re.Match(sub.units, 0)
	if err != nil || caps == nil {
		return nil, err
	}
	return []int{sub.byteAt(caps[0]), sub.byteAt(caps[1])}, nil
}

// FindStringSubmatch returns the leftmost match and what each group matched,
// or nil if the pattern does not match.
//
// A group that did not participate is an empty string, as in Go's regexp;
// FindStringSubmatchIndex tells it apart from one that matched emptiness.
func (r *Regexp) FindStringSubmatch(s string) ([]string, error) {
	sub := newSubject(s)
	caps, err := r.re.Match(sub.units, 0)
	if err != nil || caps == nil {
		return nil, err
	}
	return submatchStrings(sub, caps), nil
}

// FindStringSubmatchIndex returns the byte bounds of the leftmost match and of
// each group, or nil if the pattern does not match.
//
// The result holds two entries per group, the whole match first, with -1 for a
// group that did not participate.
func (r *Regexp) FindStringSubmatchIndex(s string) ([]int, error) {
	sub := newSubject(s)
	caps, err := r.re.Match(sub.units, 0)
	if err != nil || caps == nil {
		return nil, err
	}
	return submatchIndex(sub, caps), nil
}

// FindStringSubmatchMap returns what each named group matched.
//
// It is nil when the pattern does not match, and holds an entry for every named
// group that participated. Groups without names are left out; use
// FindStringSubmatch for those.
func (r *Regexp) FindStringSubmatchMap(s string) (map[string]string, error) {
	sub := newSubject(s)
	caps, err := r.re.Match(sub.units, 0)
	if err != nil || caps == nil {
		return nil, err
	}
	out := make(map[string]string)
	for name, i := range r.re.GroupNames() {
		if 2*i+1 < len(caps) && caps[2*i] >= 0 {
			out[name] = sub.text(caps[2*i], caps[2*i+1])
		}
	}
	return out, nil
}

func submatchStrings(sub *subject, caps []int) []string {
	out := make([]string, len(caps)/2)
	for i := range out {
		out[i] = sub.text(caps[2*i], caps[2*i+1])
	}
	return out
}

func submatchIndex(sub *subject, caps []int) []int {
	out := make([]int, len(caps))
	for i, c := range caps {
		out[i] = sub.byteAt(c)
	}
	return out
}

// ---------------------------------------------------------------------------
// Repeated matching
// ---------------------------------------------------------------------------

// eachMatch calls visit for successive non-overlapping matches, at most n of
// them, or all of them when n is negative.
//
// An empty match advances by one position, which is what keeps a pattern that
// can match nothing from finding it for ever at the same place.
func (r *Regexp) eachMatch(sub *subject, n int, visit func(caps []int)) error {
	if n == 0 {
		return nil
	}
	for pos, found := 0, 0; pos <= len(sub.units); {
		caps, err := r.re.Match(sub.units, pos)
		if err != nil {
			return err
		}
		if caps == nil {
			return nil
		}
		visit(caps)
		found++
		if n > 0 && found >= n {
			return nil
		}
		if caps[1] == caps[0] {
			pos = caps[1] + 1
		} else {
			pos = caps[1]
		}
	}
	return nil
}

// FindAllString returns up to n successive matches, or all of them when n is
// negative. It returns nil if there are none.
func (r *Regexp) FindAllString(s string, n int) ([]string, error) {
	sub := newSubject(s)
	var out []string
	err := r.eachMatch(sub, n, func(caps []int) {
		out = append(out, sub.text(caps[0], caps[1]))
	})
	return out, err
}

// FindAllStringIndex returns the byte bounds of up to n successive matches.
func (r *Regexp) FindAllStringIndex(s string, n int) ([][]int, error) {
	sub := newSubject(s)
	var out [][]int
	err := r.eachMatch(sub, n, func(caps []int) {
		out = append(out, []int{sub.byteAt(caps[0]), sub.byteAt(caps[1])})
	})
	return out, err
}

// FindAllStringSubmatch returns up to n successive matches with their groups.
func (r *Regexp) FindAllStringSubmatch(s string, n int) ([][]string, error) {
	sub := newSubject(s)
	var out [][]string
	err := r.eachMatch(sub, n, func(caps []int) {
		out = append(out, submatchStrings(sub, caps))
	})
	return out, err
}

// FindAllStringSubmatchIndex returns the byte bounds of up to n successive
// matches and of their groups.
func (r *Regexp) FindAllStringSubmatchIndex(s string, n int) ([][]int, error) {
	sub := newSubject(s)
	var out [][]int
	err := r.eachMatch(sub, n, func(caps []int) {
		out = append(out, submatchIndex(sub, caps))
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Replacing and splitting
// ---------------------------------------------------------------------------

// ReplaceAllString replaces every match with the expansion of repl.
//
// The replacement follows JavaScript's String.prototype.replace rather than Go's
// regexp, since a pattern written for JavaScript usually arrives with a
// replacement written for it:
//
//	$&        the whole match
//	$1 .. $99 the group with that number
//	$<name>   the group with that name
//	$`        the text before the match
//	$'        the text after it
//	$$        a literal dollar sign
func (r *Regexp) ReplaceAllString(s, repl string) (string, error) {
	return r.replaceAll(s, func(sub *subject, caps []int) string {
		return expand(sub, caps, r, repl)
	})
}

// ReplaceAllLiteralString replaces every match with repl, which is used as it
// stands: no dollar sign means anything.
func (r *Regexp) ReplaceAllLiteralString(s, repl string) (string, error) {
	return r.replaceAll(s, func(*subject, []int) string { return repl })
}

// ReplaceAllStringFunc replaces every match with the result of calling repl on
// the text it matched.
func (r *Regexp) ReplaceAllStringFunc(s string, repl func(match string) string) (string, error) {
	return r.replaceAll(s, func(sub *subject, caps []int) string {
		return repl(sub.text(caps[0], caps[1]))
	})
}

// ReplaceAllStringSubmatchFunc replaces every match with the result of calling
// repl on the match and its groups, which is what a replacement that needs more
// than the matched text requires.
func (r *Regexp) ReplaceAllStringSubmatchFunc(s string, repl func(groups []string) string) (string, error) {
	return r.replaceAll(s, func(sub *subject, caps []int) string {
		return repl(submatchStrings(sub, caps))
	})
}

func (r *Regexp) replaceAll(s string, repl func(*subject, []int) string) (string, error) {
	sub := newSubject(s)
	var b strings.Builder
	last := 0
	err := r.eachMatch(sub, -1, func(caps []int) {
		b.WriteString(sub.text(last, caps[0]))
		b.WriteString(repl(sub, caps))
		last = caps[1]
	})
	if err != nil {
		return "", err
	}
	b.WriteString(sub.text(last, len(sub.units)))
	return b.String(), nil
}

// expand applies the dollar forms of a replacement template.
func expand(sub *subject, caps []int, r *Regexp, tmpl string) string {
	if !strings.ContainsRune(tmpl, '$') {
		return tmpl
	}
	var b strings.Builder
	groups := len(caps)/2 - 1
	for i := 0; i < len(tmpl); i++ {
		if tmpl[i] != '$' || i+1 >= len(tmpl) {
			b.WriteByte(tmpl[i])
			continue
		}
		switch c := tmpl[i+1]; {
		case c == '$':
			b.WriteByte('$')
			i++
		case c == '&':
			b.WriteString(sub.text(caps[0], caps[1]))
			i++
		case c == '`':
			b.WriteString(sub.text(0, caps[0]))
			i++
		case c == '\'':
			b.WriteString(sub.text(caps[1], len(sub.units)))
			i++
		case c == '<':
			end := strings.IndexByte(tmpl[i+2:], '>')
			if end < 0 {
				b.WriteByte('$')
				continue
			}
			name := tmpl[i+2 : i+2+end]
			if idx := r.SubexpIndex(name); idx > 0 && caps[2*idx] >= 0 {
				b.WriteString(sub.text(caps[2*idx], caps[2*idx+1]))
			}
			i += 2 + end
		case c >= '0' && c <= '9':
			// Two digits are preferred when they name a group that exists, so
			// $12 is group 12 where there is one and group 1 followed by "2"
			// where there is not.
			idx := int(c - '0')
			used := 1
			if i+2 < len(tmpl) && tmpl[i+2] >= '0' && tmpl[i+2] <= '9' {
				if two := idx*10 + int(tmpl[i+2]-'0'); two >= 1 && two <= groups {
					idx, used = two, 2
				}
			}
			if idx < 1 || idx > groups {
				b.WriteByte('$')
				continue
			}
			if caps[2*idx] >= 0 {
				b.WriteString(sub.text(caps[2*idx], caps[2*idx+1]))
			}
			i += used
		default:
			b.WriteByte('$')
		}
	}
	return b.String()
}

// Split slices s around the matches, returning the text between them.
//
// n limits how many pieces are returned: the last one holds the rest of the
// string, and a negative n returns every piece. What a group captured is not
// included, unlike String.prototype.split, whose results a JavaScript program
// would iterate differently.
func (r *Regexp) Split(s string, n int) ([]string, error) {
	if n == 0 {
		return nil, nil
	}
	sub := newSubject(s)
	var out []string
	last := 0
	err := r.eachMatch(sub, -1, func(caps []int) {
		if n > 0 && len(out) >= n-1 {
			return
		}
		// A match of nothing at the very start splits nothing off.
		if caps[1] == 0 && caps[0] == 0 {
			return
		}
		out = append(out, sub.text(last, caps[0]))
		last = caps[1]
	})
	if err != nil {
		return nil, err
	}
	return append(out, sub.text(last, len(sub.units))), nil
}
