package regdiff

import (
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/parser"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// TestRegNativeCaches runs the register interpreter's corpus (the regDiff
// tests' programs and calls) in register mode by the native handlers and
// by the Go table's alone, and checks that the inline caches the JIT reads
// -- the property sites, the global sites' epochs, the call sites' kinds
// and callees -- end up the same (vm.RegCacheDigest): the native fast
// paths and the Go handlers they leave to fill them alike. Where the
// build has no native handlers both runs are the Go table's.
func TestRegNativeCaches(t *testing.T) {
	compared, differ, lines := 0, 0, 0
	regDiffHook = func(t *testing.T, src string, calls []string) {
		var prog strings.Builder
		prog.WriteString(src)
		for _, c := range calls {
			prog.WriteString("\n;try { (0, eval)(" + jsQuote(c) + ") } catch (e) {}")
		}
		text := prog.String()
		digest := func(native bool) (string, bool) {
			compiler.SetRegisterForm(true)
			p, err := parser.Parse(text, parser.Options{})
			if err != nil {
				compiler.SetRegisterForm(false)
				return "", false
			}
			fn, err := compiler.Compile(p, compiler.Options{Text: text})
			compiler.SetRegisterForm(false)
			if err != nil {
				return "", false
			}
			vm.SetRegTier(true)
			was := vm.SetRegNative(native)
			defer func() { vm.SetRegTier(false); vm.SetRegNative(was) }()
			r := vm.New(vm.Config{})
			defer r.Close()
			r.Run(fn)
			return vm.RegCacheDigest(r), true
		}
		goTable, ok := digest(false)
		if !ok {
			return
		}
		native, _ := digest(true)
		compared++
		lines += strings.Count(goTable, "\n")
		if native != goTable {
			differ++
			a, b := strings.Split(goTable, "\n"), strings.Split(native, "\n")
			for i := 0; i < len(a) && i < len(b); i++ {
				if a[i] != b[i] {
					t.Errorf("caches differ at line %d: the Go table's %q, native %q", i, a[i], b[i])
					return
				}
			}
			t.Errorf("caches differ: %d lines by the Go table, %d natively", len(a), len(b))
		}
	}
	defer func() { regDiffHook = nil }()
	for _, tc := range []struct {
		name string
		f    func(*testing.T)
	}{
		{"Appends", TestRegDiffAppends}, {"ArgumentsForms", TestRegDiffArgumentsForms}, {"BlockFunctions", TestRegDiffBlockFunctions},
		{"CallTargets", TestRegDiffCallTargets}, {"ChainsAndClosures", TestRegDiffChainsAndClosures}, {"CharCodeAt", TestRegDiffCharCodeAt},
		{"Classes", TestRegDiffClasses}, {"Constructions", TestRegDiffConstructions}, {"ElemWrites", TestRegDiffElemWrites},
		{"FarJumps", TestRegDiffFarJumps}, {"FarNames", TestRegDiffFarNames},
		{"FarRegisters", TestRegDiffFarRegisters}, {"FinallyCopies", TestRegDiffFinallyCopies}, {"FinallyDeep", TestRegDiffFinallyDeep},
		{"ForOfIn", TestRegDiffForOfIn}, {"GeneratorSteps", TestRegDiffGeneratorSteps}, {"Generators", TestRegDiffGenerators},
		{"Int32", TestRegDiffInt32}, {"LabelsSwitchDelete", TestRegDiffLabelsSwitchDelete},
		{"LeafRun", TestRegDiffLeafRun}, {"Length", TestRegDiffLength}, {"Literals", TestRegDiffLiterals}, {"LoopBindings", TestRegDiffLoopBindings},
		{"Microbenchmarks", TestRegDiffMicrobenchmarks}, {"Names", TestRegDiffNames}, {"ObjectLiterals", TestRegDiffObjectLiterals},
		{"Patterns", TestRegDiffPatterns}, {"RegExpLiteralCalls", TestRegDiffRegExpLiteralCalls}, {"RegExpLiterals", TestRegDiffRegExpLiterals},
		{"RegExpWideConstants", TestRegDiffRegExpWideConstants}, {"RestArguments", TestRegDiffRestArguments}, {"Sites", TestRegDiffSites},
		{"StackSitesPast16Bits", TestRegDiffStackSitesPast16Bits}, {"StacksAndNames", TestRegDiffStacksAndNames}, {"Steps", TestRegDiffSteps},
		{"StrictGlobalStores", TestRegDiffStrictGlobalStores}, {"TailCalls", TestRegDiffTailCalls}, {"TopLevel", TestRegDiffTopLevel},
		{"Try", TestRegDiffTry}, {"Typed", TestRegDiffTyped}, {"Using", TestRegDiffUsing}, {"V8Suite", TestRegDiffV8Suite},
		{"WideForms", TestRegDiffWideForms}, {"With", TestRegDiffWith},
	} {
		t.Run(tc.name, tc.f)
	}
	t.Logf("%d programs compared, %d lines of caches, %d differ", compared, lines, differ)
	if compared == 0 || lines < 1000 {
		t.Fatalf("%d programs compared, %d lines of caches: too few to tell", compared, lines)
	}
}

// jsQuote is s as a JavaScript string literal.
func jsQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, c := range s {
		switch c {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(c)
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case ' ', ' ':
			b.WriteString(`\u` + map[rune]string{' ': "2028", ' ': "2029"}[c])
		default:
			b.WriteRune(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}
