// Command regcount is a static count of the register code's patterns that
// the loop-kernel and int32 instructions would replace: it compiles every
// .js and .mjs file under the directories it is given, with the register
// form on, and counts, for each directory and then for all of them,
//
//   - a step followed by a compare-and-branch of the stepped register
//     (inc, dec, add_imm or sub_imm of a register into itself, then
//     jlt r, ... and its kin), by the branch's kind and the step's: what a
//     step-and-branch instruction would fuse, and the pairs it could not,
//     by why; and the step-and-branches regcomp made of the six kinds it
//     fuses, which leave the pairs above only the other kinds, with
//     every other loop-kernel and int32 instruction it made;
//   - x * k with k a 16-bit integer literal, right or left (mul_imm);
//   - (x + y) | 0 and (x - y) | 0 (add_i32, sub_i32), (x + k) | 0 and
//     (x - k) | 0 (add_imm_i32), the + of a string chain left out; and the
//     | 0 whose left is a bitwise operator with a number literal or a >>>
//     result for an operand, which cannot change its value;
//   - comparisons of int32s, (a | 0) < (b | 0), by operator, with one side
//     | 0 and with an immediate.
//
// The step pairs are read from the register code, as the peephole that
// fuses them would read it; the operators from the AST, as regcomp's
// binary would match them -- every function's, those regcomp refuses
// included.
//
//	go run ./internal/cmd/regcount dir...
//
// A file of test262 is compiled as its front matter says, as regrefusals
// compiles it; one that does not compile -- a negative test -- is counted
// and left out.
package main

import (
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// tally is a directory's count.
type tally struct {
	files, failed  int
	funcs, regCode int
	// evals is how many of the files are code a file evals.
	evals int
	// made is the loop-kernel and int32 instructions regcomp made (the
	// operations from step_jlt on), by operation.
	made map[string]int
	// step is the fusable step pairs by the branch's kind, then the
	// step's: "+1", "-1", "+k", "-k"; back those whose branch jumps back,
	// a step-and-branch's counted by its partner's kind.
	step map[string]map[string]int
	back map[string]int
	// stepMiss is the step pairs no fused instruction takes, by why.
	stepMiss map[string]int
	// expr is the operator patterns, by name.
	expr map[string]int
}

func newTally() *tally {
	return &tally{step: map[string]map[string]int{}, back: map[string]int{}, stepMiss: map[string]int{}, expr: map[string]int{}, made: map[string]int{}}
}

func (t *tally) add(o *tally) {
	t.files += o.files
	t.evals += o.evals
	for k, n := range o.made {
		t.made[k] += n
	}
	t.failed += o.failed
	t.funcs += o.funcs
	t.regCode += o.regCode
	for k, m := range o.step {
		if t.step[k] == nil {
			t.step[k] = map[string]int{}
		}
		for s, n := range m {
			t.step[k][s] += n
		}
	}
	for k, n := range o.back {
		t.back[k] += n
	}
	for k, n := range o.stepMiss {
		t.stepMiss[k] += n
	}
	for k, n := range o.expr {
		t.expr[k] += n
	}
}

func main() {
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: regcount dir...")
		os.Exit(2)
	}
	all := newTally()
	for _, dir := range flag.Args() {
		t := newTally()
		filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			ext := filepath.Ext(path)
			if ext != ".js" && ext != ".mjs" {
				return nil
			}
			text, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			t.countSource(path, string(text), ext == ".mjs")
			return nil
		})
		t.report(os.Stdout, dir)
		all.add(t)
	}
	if flag.NArg() > 1 {
		all.report(os.Stdout, "all")
	}
}

// countSource compiles a file and counts its patterns, or counts it as
// not compiled.
func (t *tally) countSource(path, text string, module bool) {
	t.files++
	prog, fn, ok := compile(path, text, module)
	if !ok {
		t.failed++
		return
	}
	walk(fn, func(f *bytecode.Function) {
		t.funcs++
		if rc := f.RegCode(); rc != nil {
			t.regCode++
			t.countSteps(rc)
		}
	})
	for i, code := range t.countExprs(prog) {
		t.evals++
		t.countSource(fmt.Sprintf("%s#eval%d", path, i), code, false)
	}
}

// compile compiles a file as test262's front matter, or its extension,
// says, and reports whether it compiled.
func compile(path, text string, module bool) (prog *ast.Program, fn *bytecode.Function, ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	strict := false
	if i := strings.Index(text, "/*---"); i >= 0 {
		if j := strings.Index(text[i:], "---*/"); j >= 0 {
			meta := text[i : i+j]
			if k := strings.Index(meta, "flags:"); k >= 0 {
				line := meta[k:]
				if e := strings.IndexByte(line, '\n'); e >= 0 {
					line = line[:e]
				}
				module = module || strings.Contains(line, "module")
				strict = strings.Contains(line, "onlyStrict")
			}
		}
	}
	module = module || strings.Contains(filepath.Base(path), "_FIXTURE")
	prog, err := parser.Parse(text, parser.Options{Module: module, Strict: strict && !module})
	if err != nil {
		return nil, nil, false
	}
	opts := compiler.Options{Source: path, Text: text}
	if module {
		fn, _, err = compiler.CompileModule(prog, opts)
	} else {
		fn, err = compiler.Compile(prog, opts)
	}
	return prog, fn, err == nil && fn != nil
}

// walk calls visit for fn and every function compiled inside it.
func walk(fn *bytecode.Function, visit func(*bytecode.Function)) {
	visit(fn)
	for i := range fn.Constants {
		if c := &fn.Constants[i]; c.Kind == bytecode.ConstFunction && c.Fn != nil {
			walk(c.Fn, visit)
		}
	}
}

// branchKind reports whether op is a compare-and-branch of a register
// with a register, an immediate or an upvalue, and which of those its
// right operand is.
func branchKind(op bytecode.ROp) (right string, ok bool) {
	switch op {
	case bytecode.RJLt, bytecode.RJNLt, bytecode.RJLe, bytecode.RJNLe, bytecode.RJGt, bytecode.RJNGt,
		bytecode.RJGe, bytecode.RJNGe, bytecode.RJSEq, bytecode.RJSNe, bytecode.RJEq, bytecode.RJNe:
		return "reg", true
	case bytecode.RJLtI, bytecode.RJNLtI, bytecode.RJLeI, bytecode.RJNLeI, bytecode.RJGtI, bytecode.RJNGtI,
		bytecode.RJGeI, bytecode.RJNGeI, bytecode.RJSEqI, bytecode.RJSNeI, bytecode.RJEqI, bytecode.RJNeI:
		return "imm", true
	case bytecode.RJLtU, bytecode.RJNLtU, bytecode.RJLeU, bytecode.RJNLeU:
		return "upval", true
	}
	return "", false
}

// farShaped reports whether every compare-and-branch of code is a branch
// over a jmp, as a function compiled with its jumps far has them: the
// peephole leaves such a function alone.
func farShaped(code []bytecode.RInstr) bool {
	seen := false
	for p, w := range code {
		if _, ok := branchKind(w.Op()); ok {
			if w.SC() != 1 || p+2 >= len(code) || code[p+1].Op() != bytecode.RJmp {
				return false
			}
			seen = true
		}
	}
	return seen
}

// countSteps counts rc's step pairs: code[p] steps a register in place,
// code[p+1] compares it and branches. A pair is fusable where the step
// is ±1 or ±k with k of 1 to 63 (the fused instruction's X), the
// branch's right operand is not the counter, and the fused offset, the
// branch's plus one, fits 16 bits.
func (t *tally) countSteps(rc *bytecode.RegCode) {
	code := rc.Code
	for _, w := range code {
		if w.Op() >= bytecode.RStepJLt && w.Op() < bytecode.ROpCount {
			t.made[w.Op().String()]++
		}
		if partner, ok := w.Op().StepPartner(); ok && w.SC() < 0 {
			t.back[partner.String()]++
		}
	}
	far := -1
	for p := 0; p+1 < len(code); p++ {
		s, c := code[p], code[p+1]
		var step string
		k := int32(0)
		switch s.Op() {
		case bytecode.RInc:
			step = "+1"
		case bytecode.RDec:
			step = "-1"
		case bytecode.RAddI:
			step, k = "+k", s.SC()
		case bytecode.RSubI:
			step, k = "-k", -s.SC()
		default:
			continue
		}
		right, ok := branchKind(c.Op())
		if !ok || c.A() != s.A() {
			continue
		}
		kind := c.Op().String()
		switch {
		case s.A() != s.B():
			t.stepMiss["step into another register"]++
		case step[1] == 'k' && (k == 0 || k < -64 || k > 63):
			t.stepMiss["step past 7 bits ("+kind+")"]++
		case right == "reg" && c.B() == c.A():
			t.stepMiss["bound is the counter"]++
		case c.SC() >= math.MaxInt16:
			t.stepMiss["offset past 16 bits"]++
		default:
			if far < 0 {
				far = 0
				if farShaped(code) {
					far = 1
				}
			}
			if far == 1 {
				t.stepMiss["far jumps"]++
				continue
			}
			if t.step[kind] == nil {
				t.step[kind] = map[string]int{}
			}
			t.step[kind][step]++
			if c.SC() < 0 {
				t.back[kind]++
			}
		}
	}
}

// numberLit is e's value where it is a number literal or the negation of
// one, as regcomp's numberLit.
func numberLit(e ast.Expr) (float64, bool) {
	switch n := e.(type) {
	case *ast.NumberLit:
		return n.Value, true
	case *ast.Unary:
		if lit, ok := n.Operand.(*ast.NumberLit); ok && n.Op == "-" {
			return -lit.Value, true
		}
	}
	return 0, false
}

// imm16 is e's value where it is an integer literal of 16 bits, -0 not
// one, as regcomp's imm16.
func imm16(e ast.Expr) (int32, bool) {
	v, ok := numberLit(e)
	if !ok {
		return 0, false
	}
	i := int32(v)
	return i, float64(i) == v && !(v == 0 && math.Signbit(v)) && i >= -1<<15 && i < 1<<15
}

// isZero reports whether e is the literal 0.
func isZero(e ast.Expr) bool {
	v, ok := numberLit(e)
	return ok && v == 0
}

// orZero is x where e is x | 0.
func orZero(e ast.Expr) (ast.Expr, bool) {
	if b, ok := e.(*ast.Binary); ok && b.Op == "|" && isZero(b.Right) {
		return b.Left, true
	}
	return nil, false
}

// stringChain reports whether a + chain has a string literal or a
// template in it, which regcomp's stringChain compiles as a concat.
func stringChain(e ast.Expr) bool {
	switch n := e.(type) {
	case *ast.StringLit, *ast.TemplateLit:
		return true
	case *ast.Binary:
		return n.Op == "+" && (stringChain(n.Left) || stringChain(n.Right))
	}
	return false
}

// int32Result reports whether e is a bitwise operator whose result is an
// int32 Number whenever it does not throw: one of its operands a number
// literal or a >>> result, which cannot be a BigInt.
func int32Result(e ast.Expr) bool {
	b, ok := e.(*ast.Binary)
	if !ok {
		return false
	}
	switch b.Op {
	case "&", "|", "^", "<<", ">>":
	default:
		return false
	}
	known := func(x ast.Expr) bool {
		if _, ok := numberLit(x); ok {
			return true
		}
		u, ok := x.(*ast.Binary)
		return ok && u.Op == ">>>"
	}
	return known(b.Left) || known(b.Right)
}

// countExprs counts the operator patterns of every expression of prog,
// and returns the code it evals: the string literal, of 1,000 characters
// or more, that is the one argument of a call of a name with eval in it --
// Octane's zlib, whose code is the string zlibEval, eval, runs.
func (t *tally) countExprs(prog *ast.Program) (evals []string) {
	type key struct {
		t reflect.Type
		p uintptr
	}
	seen := map[key]bool{}
	var visit func(v reflect.Value)
	visit = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Pointer:
			k := key{v.Type(), v.Pointer()}
			if v.IsNil() || seen[k] {
				return
			}
			seen[k] = true
			if v.Type().Elem().Kind() != reflect.Struct {
				return
			}
			switch n := v.Interface().(type) {
			case *ast.Binary:
				t.binary(n)
			case *ast.Assign:
				t.assign(n)
			case *ast.Call:
				id, ok := n.Callee.(*ast.Ident)
				if ok && len(n.Args) == 1 && strings.Contains(strings.ToLower(id.Name), "eval") {
					if s, ok := n.Args[0].(*ast.StringLit); ok && len(s.Value) >= 1000 {
						evals = append(evals, s.Value)
					}
				}
			}
			visit(v.Elem())
		case reflect.Interface:
			if !v.IsNil() {
				visit(v.Elem())
			}
		case reflect.Struct:
			for i := range v.NumField() {
				if f := v.Field(i); f.CanInterface() {
					visit(f)
				}
			}
		case reflect.Slice, reflect.Array:
			if v.Kind() == reflect.Slice && v.IsNil() {
				return
			}
			if k := v.Type().Elem().Kind(); k != reflect.Pointer && k != reflect.Interface && k != reflect.Struct && k != reflect.Slice {
				return
			}
			for i := range v.Len() {
				visit(v.Index(i))
			}
		}
	}
	visit(reflect.ValueOf(prog))
	return evals
}

// mul counts x * k and k * x.
func (t *tally) mul(left, right ast.Expr) {
	if _, ok := imm16(right); ok {
		t.expr["mul: x * k"]++
		return
	}
	if _, ok := numberLit(right); ok {
		t.expr["mul: x * k, k not 16-bit int"]++
		return
	}
	if _, ok := imm16(left); ok {
		t.expr["mul: k * x"]++
		return
	}
	if _, ok := numberLit(left); ok {
		t.expr["mul: k * x, k not 16-bit int"]++
	}
}

// orZero counts x | 0 by what x is.
func (t *tally) orZeroOf(x ast.Expr) {
	t.expr["or0: all x | 0"]++
	inner, ok := x.(*ast.Binary)
	switch {
	case ok && (inner.Op == "+" || inner.Op == "-"):
		if inner.Op == "+" && stringChain(inner) {
			t.expr["or0: (string chain) | 0"]++
			return
		}
		if k, ok := imm16(inner.Right); ok && (inner.Op == "+" || k != -1<<15) {
			t.expr["or0: (x "+inner.Op+" k) | 0  -> add_imm_i32"]++
			return
		}
		name := map[string]string{"+": "add_i32", "-": "sub_i32"}[inner.Op]
		t.expr["or0: (x "+inner.Op+" y) | 0  -> "+name]++
	case int32Result(x):
		t.expr["or0: (int32 bitwise) | 0, identity"]++
	case ok && (inner.Op == "&" || inner.Op == "|" || inner.Op == "^" || inner.Op == "<<" || inner.Op == ">>"):
		t.expr["or0: (x bitwise y) | 0, not provable"]++
	case ok && inner.Op == "*":
		t.expr["or0: (x * y) | 0"]++
	}
}

func (t *tally) binary(n *ast.Binary) {
	switch n.Op {
	case "*":
		t.mul(n.Left, n.Right)
	case "|":
		if isZero(n.Right) {
			t.orZeroOf(n.Left)
		}
	case "<", "<=", ">", ">=":
		_, l := orZero(n.Left)
		_, r := orZero(n.Right)
		_, li := imm16(n.Left)
		_, ri := imm16(n.Right)
		switch {
		case l && r:
			t.expr["cmp: (a|0) "+n.Op+" (b|0)"]++
		case l && ri || r && li:
			t.expr["cmp: (a|0) "+n.Op+" k"]++
		case l || r:
			t.expr["cmp: (a|0) "+n.Op+" b"]++
		}
	}
}

func (t *tally) assign(n *ast.Assign) {
	switch n.Op {
	case "*=":
		t.mul(n.Target, n.Value)
	case "|=":
		if isZero(n.Value) {
			t.expr["or0: all x | 0"]++
		}
	}
}

// report prints t under the name dir.
func (t *tally) report(w io.Writer, dir string) {
	fmt.Fprintf(w, "%s: %d files (%d of them evaled code, %d not compiled), %d functions, %d as register code\n",
		dir, t.files, t.evals, t.failed, t.funcs, t.regCode)
	var kinds []string
	total := 0
	for k, m := range t.step {
		kinds = append(kinds, k)
		for _, n := range m {
			total += n
		}
	}
	sort.Slice(kinds, func(i, j int) bool {
		a, b := sum(t.step[kinds[i]]), sum(t.step[kinds[j]])
		if a != b {
			return a > b
		}
		return kinds[i] < kinds[j]
	})
	fmt.Fprintf(w, "  step pairs, fusable: %d\n", total)
	fmt.Fprintf(w, "    %-14s %7s %7s %7s %7s %7s %7s\n", "branch", "all", "back", "+1", "-1", "+k", "-k")
	for _, k := range kinds {
		m := t.step[k]
		fmt.Fprintf(w, "    %-14s %7d %7d %7d %7d %7d %7d\n", k, sum(m), t.back[k], m["+1"], m["-1"], m["+k"], m["-k"])
	}
	printSorted(w, "  loop-kernel and int32 instructions made:", t.made)
	printSorted(w, "  step pairs, not fusable:", t.stepMiss)
	printSorted(w, "  operators:", t.expr)
}

func sum(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// printSorted prints m's entries by name, under title.
func printSorted(w io.Writer, title string, m map[string]int) {
	fmt.Fprintln(w, title)
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "    %-44s %8d\n", k, m[k])
	}
}
