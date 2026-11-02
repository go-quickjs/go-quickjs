//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package verify

import (
	"math/rand/v2"
	"strings"
	"testing"

	jsregexp "github.com/go-quickjs/go-quickjs/internal/regexp"
)

// regexpCorpus reaches every instruction the regular expressions' native
// code compiles, over bytes and over UTF-16, with and without the u flag.
var regexpCorpus = []struct{ pattern, flags string }{
	{`abc`, ""}, {`abcdefghijklmnopq`, ""}, {`a.c`, "s"}, {`[a-c]+`, "i"}, {`[^a-c]+`, ""}, {`\d+\.\d*`, ""},
	{`\w+\s+\W`, "iu"}, {`a|b|cd`, ""}, {`(a|ab)(c|bcd)(d*)`, ""}, {`a*?b`, ""}, {`.+?x`, "u"},
	{`a{2,4}`, ""}, {`(ab){1,3}c`, ""}, {`(?:a|b){3,5}?c`, ""}, {`(a*)*b`, ""}, {`((a)|(b))+`, ""},
	{`(a+)\1`, ""}, {`(ab)\1`, "i"}, {`(?:(?<x>a)|(?<x>b))\k<x>`, ""}, {`^a$`, "m"}, {`\bab\B`, ""},
	{`é+`, ""}, {`[à-ÿ]+`, "i"}, {`\p{L}+`, "u"}, {`\P{L}`, "iu"}, {`😀+`, "u"}, {`[^😀]`, "u"}, {`.`, "u"},
	{`[\uD800-\uDBFF]`, "u"}, {`\p{Cs}`, "u"}, {`(.)\1`, "u"}, {`[\p{L}\p{N}]+`, "u"}, {`K`, "iu"},
	{`[一-鿿]+`, ""}, {`(?:é|è){2,3}`, ""}, {`"[^"]*"`, ""}, {`^.$`, "mu"}, {`\S+$`, "u"},
	// The search around the attempts: a unit or either of two passed over a
	// word at a time, a class's units one at a time, under the unicode
	// flag a character at a time, sticky, the required literal, and the
	// captures, counters and marks reset by a loop.
	{`x\d`, ""}, {`[xy]\d`, "y"}, {`n`, "i"}, {`[éa]b`, "u"}, {`[a😀]x`, "u"}, {`\d{1,3}px`, ""}, {`^ab`, ""},
	{`(a)(b)(c)(d)(e)(f)(g)(h)(i)`, ""}, {`(?:(?:(a)*b){2,3}(?:(c)*d){2}){1,2}`, ""},
}

// Every pattern's native code, for each machine and each kind of subject,
// decodes in full and keeps to the registers native code may write.
func TestRegExpNativeEncodings(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	corpus := regexpCorpus
	atoms := []string{"a", "é", ".", "[ab]", `\w`, `\p{L}`, "😀", `\b`, "^", "$", "(a)", `\1`}
	for i := 0; i < 300; i++ {
		var b strings.Builder
		for n := 1 + r.IntN(5); n > 0; n-- {
			b.WriteString(atoms[r.IntN(len(atoms))])
			b.WriteString([]string{"", "*", "+?", "{1,3}", "|"}[r.IntN(5)])
		}
		corpus = append(corpus, struct{ pattern, flags string }{b.String(), []string{"", "i", "u", "iu", "m"}[r.IntN(5)]})
	}
	compiled := 0
	for _, arch := range arches {
		for _, wide := range []bool{false, true} {
			for _, p := range corpus {
				code, text, ok := jsregexp.NativeListing(p.pattern, p.flags, arch, wide)
				if !ok {
					continue
				}
				compiled++
				if err := check(arch, code[:text]); err != nil {
					t.Fatalf("/%s/%s on %s (wide %v): %v", p.pattern, p.flags, arch, wide, err)
				}
			}
		}
	}
	if compiled < 4*len(regexpCorpus) {
		t.Fatalf("%d compiled", compiled)
	}
}
