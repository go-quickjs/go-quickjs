package compile

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// regLoweringOf is what the register lowering makes of fn, as ssaRefusal
// tells the stack form's: "lowered" for Go's entry (LowerReg); for a
// function the entry leaves for doing no loop's or array's work, "lowered
// callee" or "callee: <reason>" (LowerRegInline), or, a generator's or an
// async function's, "lowered hot" or "hot: <reason>" (LowerRegHot); and
// "entry: <reason>" for one the entry refuses outright. It returns the
// programs it made, for their validation.
func regLoweringOf(fn *bytecode.Function) (string, []error) {
	if fn.RegCode() == nil {
		return "entry: no register code", nil
	}
	reason := func(err error) string {
		var rf *Refusal
		if errors.As(err, &rf) {
			return rf.Reason
		}
		return "error: " + err.Error()
	}
	var invalid []error
	check := func(lower func(*bytecode.RegCode, *bytecode.Function) (*ir.Program, error)) error {
		p, err := lower(fn.RegCode(), fn)
		if err == nil {
			if verr := p.Validate(); verr != nil {
				invalid = append(invalid, fmt.Errorf("%s: %w", fn.Name, verr))
			}
		}
		return err
	}
	err := check(LowerReg)
	switch {
	case err == nil:
		return "lowered", invalid
	case HostOnly(err) && (fn.Async || fn.Generator):
		if err := check(LowerRegHot); err != nil {
			return "hot: " + reason(err), invalid
		}
		return "lowered hot", invalid
	case HostOnly(err):
		if err := check(LowerRegInline); err != nil {
			return "callee: " + reason(err), invalid
		}
		return "lowered callee", invalid
	}
	return "entry: " + reason(err), invalid
}

// loweredFor is the program LowerSSA's chain makes of fn (ssaRefusal's:
// the entry's, else, for host-only code, the hot or the callee's
// lowering), or, reg, LowerReg's; nil where it refuses.
func loweredFor(fn *bytecode.Function, reg bool) *ir.Program {
	type lower func(*bytecode.Function) (*ir.Program, error)
	chain := [3]lower{LowerSSA, LowerSSAHot, LowerSSAInline}
	if reg {
		if fn.RegCode() == nil {
			return nil
		}
		on := func(l func(*bytecode.RegCode, *bytecode.Function) (*ir.Program, error)) lower {
			return func(fn *bytecode.Function) (*ir.Program, error) { return l(fn.RegCode(), fn) }
		}
		chain = [3]lower{on(LowerReg), on(LowerRegHot), on(LowerRegInline)}
	}
	p, err := chain[0](fn)
	switch {
	case err == nil:
	case HostOnly(err) && (fn.Async || fn.Generator):
		p, err = chain[1](fn)
	case HostOnly(err):
		p, err = chain[2](fn)
	}
	if err != nil {
		return nil
	}
	return p
}

// corpusEncoding is a value encoding the VM's is like, for the corpus's
// compiles to machine code, which nothing runs.
var corpusEncoding = abi.Encoding{ValueSize: 16, NumOffset: 0, RefOffset: 8,
	Undefined: 0xFFF8000000000001, Null: 0xFFF8000000000002, True: 0xFFF8000000000103,
	False: 0xFFF8000000000003, Uninitialized: 0xFFF8000000000008, CanonicalNaN: 0x7FF8000000000000,
	Mega: true, UndefinedCell: 0x60000, StoreRef: 0x70000}

// regPC is an error's register pc.
var regPC = regexp.MustCompile(`register pc ([0-9]+)`)

// pcIn takes the pcs out of an error's text, which is then its kind.
var pcIn = regexp.MustCompile(`(register )?pc -?[0-9]+`)

// ssaBuilt is what ssa.Build makes of the program fn's lowering makes, and
// mir of that, or "" where the lowering refuses: "built", "ssa: <reason>"
// or "mir: <reason>".
func ssaBuilt(fn *bytecode.Function, reg bool) string {
	p := loweredFor(fn, reg)
	if p == nil {
		return ""
	}
	if int64(len(p.Code))*int64(max(p.Locals+p.StackSize, 256))*8+32<<10 > 32<<20 {
		// Past what the VM compiles at v8bench's budget of 32 MiB (its
		// jitCompileBytes, of the stack form's slots and instructions).
		return "past the compile budget"
	}
	f, err := ssa.BuildWith(p, nil)
	if err == nil {
		// And its machine code, amd64's, as the VM compiles it.
		ssa.Optimize(f)
		if _, err := mir.CompileAMD64(f, corpusEncoding); err != nil {
			return "mir: " + pcIn.ReplaceAllString(err.Error(), "pc N")
		}
	}
	if err != nil {
		why := pcIn.ReplaceAllString(err.Error(), "pc N")
		if m := regPC.FindStringSubmatch(err.Error()); reg && m != nil {
			// Which register operation, by its name.
			if pc, _ := strconv.Atoi(m[1]); pc < len(fn.RegCode().Code) {
				w := fn.RegCode().Code[pc]
				why += " (" + w.Op().String() + ")"
			}
		}
		return why
	}
	return "built"
}

// TestLowerRegCorpusRefusals is Stage 3a-2's gate (docs/register-
// interpreter.md, Stage 3, section 6): every function of the .js files
// under each directory QUICKJS_JIT_REG_REFUSALS lists (a path list: the V8
// suite's, Octane's, bundles') compiled with the register backend on, and
// lowered both ways, the stack form's (ssaRefusal) and the register code's
// (regLoweringOf). It prints how many each lowers and refuses, by reason,
// and fails where the register lowering refuses more than the stack
// form's, or makes a program ir.Validate rejects.
func TestLowerRegCorpusRefusals(t *testing.T) {
	dirs := filepath.SplitList(os.Getenv("QUICKJS_JIT_REG_REFUSALS"))
	if len(dirs) == 0 {
		t.Skip("QUICKJS_JIT_REG_REFUSALS names no directories of JavaScript files")
	}
	compiler.SetRegisterForm(true)
	defer compiler.SetRegisterForm(false)
	for _, dir := range dirs {
		stack, regs := map[string]int{}, map[string]int{}
		// What the SSA builder makes, without feedback, of the programs
		// both lowerings make.
		stackSSA, regSSA := map[string]int{}, map[string]int{}
		var invalid []error
		files, total, budgetLogs := 0, 0, 0
		var walk func(fn *bytecode.Function, count bool)
		walk = func(fn *bytecode.Function, count bool) {
			if count && !fn.TopLevel {
				total++
				stack[ssaRefusal(fn)]++
				r, bad := regLoweringOf(fn)
				regs[r]++
				invalid = append(invalid, bad...)
				if k := ssaBuilt(fn, false); k != "" {
					stackSSA[k]++
				}
				if k := ssaBuilt(fn, true); k != "" {
					regSSA[k]++
					if sk := ssaBuilt(fn, false); sk != "" && k != sk && budgetLogs < 20 {
						budgetLogs++
						p, q := loweredFor(fn, false), loweredFor(fn, true)
						t.Logf("%s: stack form %s, %d instructions of %d slots; register code %s, %d of %d (%d registers)",
							fn.Name, ssaBuilt(fn, false), len(p.Code), p.Locals+p.StackSize, k, len(q.Code), q.Locals, q.Regs)
					}
				}
			}
			for _, c := range fn.Constants {
				if c.Kind == bytecode.ConstFunction && c.Fn != nil {
					walk(c.Fn, true)
				}
			}
		}
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".js") {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			// Each file as a script's code is: its functions are what
			// count, the top level's own code not.
			text := string(b)
			prog, err := parser.Parse(text, parser.Options{})
			if err != nil {
				t.Logf("%s: %v", p, err)
				return nil
			}
			fn, err := compiler.Compile(prog, compiler.Options{Source: p, Text: text})
			if err != nil {
				t.Logf("%s: %v", p, err)
				return nil
			}
			files++
			walk(fn, false)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		refused := func(m map[string]int) int {
			n := 0
			for k, v := range m {
				if !strings.HasPrefix(k, "lowered") {
					n += v
				}
			}
			return n
		}
		var out strings.Builder
		fmt.Fprintf(&out, "%s: %d files, %d functions; refused: stack form %d, register code %d\n", dir, files, total, refused(stack), refused(regs))
		keys := map[string]bool{}
		for k := range stack {
			keys[k] = true
		}
		for k := range regs {
			keys[k] = true
		}
		rows := slices.Collect(func(yield func(string) bool) {
			for k := range keys {
				if !yield(k) {
					return
				}
			}
		})
		slices.SortFunc(rows, func(a, b string) int {
			if c := cmp.Compare(max(stack[b], regs[b]), max(stack[a], regs[a])); c != 0 {
				return c
			}
			return strings.Compare(a, b)
		})
		fmt.Fprintf(&out, "%7s %7s  %s\n", "stack", "reg", "what")
		for _, k := range rows {
			fmt.Fprintf(&out, "%7d %7d  %s\n", stack[k], regs[k], k)
		}
		fmt.Fprintf(&out, "SSA builds and amd64 compiles, without feedback, of what each lowered: refused, stack form %d, register code %d\n", refusedSSA(stackSSA), refusedSSA(regSSA))
		for _, k := range sortedKeys(stackSSA, regSSA) {
			fmt.Fprintf(&out, "%7d %7d  %s\n", stackSSA[k], regSSA[k], k)
		}
		t.Log("\n" + out.String())
		if refusedSSA(regSSA) > refusedSSA(stackSSA) {
			t.Errorf("%s: the SSA builder refuses %d register programs, %d of the stack form's", dir, refusedSSA(regSSA), refusedSSA(stackSSA))
		}
		for i, err := range invalid {
			if i == 20 {
				t.Errorf("and %d more", len(invalid)-20)
				break
			}
			t.Errorf("invalid register lowering: %v", err)
		}
		if refused(regs) > refused(stack) {
			t.Errorf("%s: the register lowering refuses %d functions, the stack form's %d", dir, refused(regs), refused(stack))
		}
	}
}

// refusedSSA is how many of m's builds the builder refused: not those
// past the compile budget, which the VM does not ask for.
func refusedSSA(m map[string]int) int {
	n := 0
	for k, v := range m {
		if k != "built" && k != "past the compile budget" {
			n += v
		}
	}
	return n
}

// sortedKeys are the keys of a and b, the most common first.
func sortedKeys(a, b map[string]int) []string {
	var ks []string
	for k := range a {
		ks = append(ks, k)
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			ks = append(ks, k)
		}
	}
	slices.SortFunc(ks, func(x, y string) int {
		if c := cmp.Compare(max(a[y], b[y]), max(a[x], b[x])); c != 0 {
			return c
		}
		return strings.Compare(x, y)
	})
	return ks
}
