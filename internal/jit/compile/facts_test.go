package compile

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// The scans of the code that the compiler's record of it replaced
// (bytecode.Function's FrameCallee, ReadsMaker, AssignsParams,
// MakesArguments, SuperForwards and HasYieldStar, UpvalueDesc's Assigned),
// kept as they were, the oracles TestRecordedFactsMatchScans holds the
// record to.

// scanFrameCallee is FrameCallee as it scanned the code.
func scanFrameCallee(fn *bytecode.Function) bool {
	if fn.MappedArguments || fn.Kind == bytecode.KindArrow && fn.UsesNewTarget {
		return true
	}
	for _, in := range fn.Code {
		if superRef(in.Op) || in.Op == bytecode.OpPushCallee {
			return true
		}
	}
	return scanMakesMakerArrow(fn)
}

func scanMakesMakerArrow(fn *bytecode.Function) bool {
	for _, k := range fn.Constants {
		if k.Kind != bytecode.ConstFunction || k.Fn == nil || k.Fn.Kind != bytecode.KindArrow {
			continue
		}
		if k.Fn.UsesNewTarget || scanMakesMakerArrow(k.Fn) {
			return true
		}
		for _, in := range k.Fn.Code {
			if superRef(in.Op) || in.Op == bytecode.OpSuperCall || in.Op == bytecode.OpNewTarget {
				return true
			}
		}
	}
	return false
}

// scanNeedsMaker is the VM's jitNeedsMaker of a closure of fn as it
// scanned the code.
func scanNeedsMaker(fn *bytecode.Function) bool {
	if fn.Kind != bytecode.KindArrow {
		return false
	}
	if fn.UsesNewTarget {
		return true
	}
	var reads func(fn *bytecode.Function) bool
	reads = func(fn *bytecode.Function) bool {
		for _, in := range fn.Code {
			switch in.Op {
			case bytecode.OpNewTarget, bytecode.OpSuperCall, bytecode.OpGetSuperProp, bytecode.OpGetSuperIndex,
				bytecode.OpSetSuperProp, bytecode.OpSetSuperIndex, bytecode.OpSuperBase:
				return true
			}
		}
		for _, k := range fn.Constants {
			if k.Kind == bytecode.ConstFunction && k.Fn != nil && k.Fn.Kind == bytecode.KindArrow && reads(k.Fn) {
				return true
			}
		}
		return false
	}
	return reads(fn)
}

// scanArgumentsOwn is argumentsOwn as it scanned the code.
func scanArgumentsOwn(fn *bytecode.Function) bool {
	if fn.Leaf == bytecode.LeafForward || fn.ForwardsArguments && scanHas(fn, func(in bytecode.Instr) bool { return in.Op == bytecode.OpApplyArguments }) {
		return false
	}
	if !fn.MappedArguments || fn.ParamCount == 0 {
		return true
	}
	for _, in := range fn.Code {
		switch in.Op {
		case bytecode.OpGetArguments, bytecode.OpLazyArguments, bytecode.OpApplyArguments:
			return false
		}
	}
	// Parameters stored to, or captured, are passed to Go's reads
	// (ArgumentParams), as many as a call of Go takes.
	return fn.ParamCount+2 <= abi.MaxGoArgs
}

// scanUpvalueAssigned is bytecode's UpvalueAssigned, which scanned t's
// code and its closures' for each upvalue asked.
func scanUpvalueAssigned(t *bytecode.Function, up int) bool {
	if t.HasDirectEval {
		return true
	}
	for _, in := range t.Code {
		switch in.Op {
		case bytecode.OpSetUpvalue, bytecode.OpSetUpvalueCheck, bytecode.OpInitUpvalue:
			if int(in.A) == up {
				return true
			}
		case bytecode.OpDirectEval, bytecode.OpTailDirectEval:
			return true
		}
	}
	for _, c := range t.Constants {
		if c.Kind != bytecode.ConstFunction || c.Fn == nil {
			continue
		}
		for j, d := range c.Fn.Upvalues {
			if !d.FromParent && int(d.Index) == up && scanUpvalueAssigned(c.Fn, j) {
				return true
			}
		}
	}
	return false
}

// scanClosureAssigned is closureAssigned as it asked scanUpvalueAssigned.
func scanClosureAssigned(fn *bytecode.Function) []bool {
	var out []bool
	for _, c := range fn.Constants {
		if c.Kind != bytecode.ConstFunction || c.Fn == nil {
			continue
		}
		for j, d := range c.Fn.Upvalues {
			if d.FromParent && int(d.Index) < fn.LocalCount && scanUpvalueAssigned(c.Fn, j) {
				if out == nil {
					out = make([]bool, fn.LocalCount)
				}
				out[d.Index] = true
			}
		}
	}
	return out
}

// scanGeneratorResumable is GeneratorResumable as it scanned the code.
func scanGeneratorResumable(fn *bytecode.Function) bool {
	if fn == nil || !fn.Generator || fn.Async || fn.HasTailCall || fn.HasDirectEval {
		return false
	}
	for _, l := range fn.Locals {
		if l.Captured {
			return false
		}
	}
	for _, in := range fn.Code {
		switch in.Op {
		case bytecode.OpYieldStar, bytecode.OpAwait:
			return false
		}
	}
	return true
}

// scanHas reports whether an instruction of fn's satisfies p.
func scanHas(fn *bytecode.Function, p func(bytecode.Instr) bool) bool {
	return slices.ContainsFunc(fn.Code, p)
}

// factCounts counts, of the functions compared, those each fact holds of.
type factCounts struct {
	files, functions, upvalues                                       int
	frameCallee, readsMaker, assignsParams, makesArguments, forwards int
	yieldStar, argumentsOwn, assigned, closureAssigned, resumable    int
}

func (c *factCounts) add(o factCounts) {
	c.files += o.files
	c.functions += o.functions
	c.upvalues += o.upvalues
	c.frameCallee += o.frameCallee
	c.readsMaker += o.readsMaker
	c.assignsParams += o.assignsParams
	c.makesArguments += o.makesArguments
	c.forwards += o.forwards
	c.yieldStar += o.yieldStar
	c.argumentsOwn += o.argumentsOwn
	c.assigned += o.assigned
	c.closureAssigned += o.closureAssigned
	c.resumable += o.resumable
}

func (c factCounts) String() string {
	return fmt.Sprintf("%d files, %d functions, %d upvalues; true: FrameCallee %d, ReadsMaker %d, AssignsParams %d, MakesArguments %d, "+
		"SuperForwards %d, HasYieldStar %d, argumentsOwn %d, Assigned %d, closureAssigned %d, GeneratorResumable %d",
		c.files, c.functions, c.upvalues, c.frameCallee, c.readsMaker, c.assignsParams, c.makesArguments,
		c.forwards, c.yieldStar, c.argumentsOwn, c.assigned, c.closureAssigned, c.resumable)
}

// checkFacts compares what the compiler recorded of fn, and of each
// function it holds, with what the scans find, returning the first
// difference, or "" for none.
func checkFacts(path string, fn *bytecode.Function, n *factCounts) string {
	n.functions++
	at := func(what string, got, want any) string {
		return fmt.Sprintf("%s (%s): %s %v, the scan's %v", path, fn.Name, what, got, want)
	}
	if got, want := fn.FrameCallee, scanFrameCallee(fn); got != want {
		return at("FrameCallee", got, want)
	}
	if got, want := fn.ReadsMaker, scanNeedsMaker(fn); got != want {
		return at("ReadsMaker", got, want)
	}
	if want := fn.Kind == bytecode.KindArrow && (fn.UsesNewTarget || scanMakesMakerArrow(fn) || scanHas(fn, func(in bytecode.Instr) bool {
		return superRef(in.Op) || in.Op == bytecode.OpSuperCall || in.Op == bytecode.OpNewTarget
	})); fn.ReadsMaker != want {
		return at("ReadsMaker as makesMakerArrow asked it", fn.ReadsMaker, want)
	}
	if got, want := fn.AssignsParams, scanHas(fn, func(in bytecode.Instr) bool {
		lo, hi := bytecode.WrittenLocals(in)
		return lo < hi && lo < fn.ParamCount
	}); got != want {
		return at("AssignsParams", got, want)
	}
	if got, want := fn.MakesArguments, scanHas(fn, func(in bytecode.Instr) bool {
		return in.Op == bytecode.OpGetArguments || in.Op == bytecode.OpLazyArguments || in.Op == bytecode.OpApplyArguments
	}); got != want {
		return at("MakesArguments", got, want)
	}
	if got, want := fn.SuperForwards, scanHas(fn, func(in bytecode.Instr) bool {
		return in.Op == bytecode.OpSuperCall && in.B == bytecode.SuperForward
	}); got != want {
		return at("SuperForwards", got, want)
	}
	if fn.SuperForwards && fn.Kind != bytecode.KindDerivedConstructor {
		// The VM's jitFrameArgs asks it of a derived constructor alone,
		// the lowering's of any function.
		return at("SuperForwards outside a derived constructor", true, false)
	}
	if got, want := fn.HasYieldStar, scanHas(fn, func(in bytecode.Instr) bool { return in.Op == bytecode.OpYieldStar }); got != want {
		return at("HasYieldStar", got, want)
	}
	if got, want := GeneratorResumable(fn), scanGeneratorResumable(fn); got != want {
		return at("GeneratorResumable", got, want)
	}
	if got, want := argumentsOwn(fn), scanArgumentsOwn(fn); got != want {
		return at("argumentsOwn", got, want)
	}
	if got, want := closureAssigned(fn), scanClosureAssigned(fn); !slices.Equal(got, want) || (got == nil) != (want == nil) {
		return at("closureAssigned", got, want)
	}
	for j, d := range fn.Upvalues {
		n.upvalues++
		if want := scanUpvalueAssigned(fn, j); d.Assigned != want {
			return at(fmt.Sprintf("upvalue %d (%s) Assigned", j, d.Name), d.Assigned, want)
		}
		if d.Assigned {
			n.assigned++
		}
	}
	count := func(b bool, c *int) {
		if b {
			*c++
		}
	}
	count(fn.FrameCallee, &n.frameCallee)
	count(fn.ReadsMaker, &n.readsMaker)
	count(fn.AssignsParams, &n.assignsParams)
	count(fn.MakesArguments, &n.makesArguments)
	count(fn.SuperForwards, &n.forwards)
	count(fn.HasYieldStar, &n.yieldStar)
	count(fn.MappedArguments && fn.ParamCount != 0 && argumentsOwn(fn), &n.argumentsOwn)
	count(closureAssigned(fn) != nil, &n.closureAssigned)
	count(GeneratorResumable(fn), &n.resumable)
	for i, k := range fn.Constants {
		if k.Kind == bytecode.ConstFunction && k.Fn != nil {
			if d := checkFacts(fmt.Sprintf("%s/%d", path, i), k.Fn, n); d != "" {
				return d
			}
		}
	}
	return ""
}

// factModes are the ways a file is compiled for TestRecordedFactsMatchScans,
// compilediff's.
var factModes = []struct {
	name string
	p    parser.Options
	c    compiler.Options
}{
	{name: "script"},
	{name: "strict", p: parser.Options{Strict: true}},
	{name: "module", p: parser.Options{Module: true}},
	{name: "debug", c: compiler.Options{Debug: true}},
	{name: "moddebug", p: parser.Options{Module: true}, c: compiler.Options{Debug: true}},
	{name: "quirks", p: parser.Options{NodeQuirks: true}, c: compiler.Options{NodeQuirks: true}},
	{name: "eval", p: parser.Options{AllowSuperProp: true, AllowNewTarget: true}, c: compiler.Options{
		EvalScope: []bytecode.EvalBinding{
			{Name: "a", FromLocal: true, Index: 0, Mutable: true, VarScoped: true},
			{Name: "x", FromLocal: true, Index: 1, Mutable: true, TDZ: true, Lexical: true},
			{Name: "f", Index: 0, FuncSelf: true},
			{Name: "arguments", FromLocal: true, Index: 2, Mutable: true, VarScoped: true},
		},
		EvalOwnVarScope: true, EvalConfigurable: true, AllowSuperProp: true, AllowNewTarget: true,
	}},
}

// compileFacts compiles text in mode m and checks the facts of every
// function it makes; a text the mode rejects counts nothing.
func compileFacts(name, text string, m int, n *factCounts) (diff string) {
	defer func() {
		if e := recover(); e != nil {
			diff = fmt.Sprintf("%s: panic: %v", name, e)
		}
	}()
	mode := factModes[m]
	prog, err := parser.Parse(text, mode.p)
	if err != nil {
		return ""
	}
	opts := mode.c
	opts.Source, opts.Text = name, text
	var fns []*bytecode.Function
	if mode.p.Module {
		fn, info, err := compiler.CompileModule(prog, opts)
		if err != nil {
			return ""
		}
		fns = append(fns, fn)
		if info.Init != nil {
			fns = append(fns, info.Init)
		}
	} else {
		fn, err := compiler.Compile(prog, opts)
		if err != nil {
			return ""
		}
		fns = append(fns, fn)
	}
	n.files++
	for i, fn := range fns {
		if d := checkFacts(fmt.Sprintf("%s[%s]#%d", name, mode.name, i), fn, n); d != "" {
			return d
		}
	}
	return ""
}

// factSources are what TestRecordedFactsMatchScans compiles without a
// corpus: each fact true and false, through arrows, closures, evals and
// classes.
var factSources = []string{
	`function f(a, b) { return arguments[0] + arguments.length }`,
	`function f(a, b) { a = 1; return arguments[0] }`,
	`function f(a, b) { var o = {}; o[++a]; return arguments[0] }`,
	`function f(a, b) { return g.apply(this, arguments) }`,
	`function f(a, b) { var x = arguments; return x }`,
	`function f(a) { return () => a++ }`,
	`function f(a) { return () => () => { a = 2 } }`,
	`function f(a) { return function () { return () => eval("a = 1") } }`,
	`"use strict"; function f(a) { return () => a } function g(a) { a = 1; return () => a }`,
	`"use strict"; function f(a) { return () => eval("a") }`,
	`function f(a) { eval(""); return () => a }`,
	`function f(a) { return function () { let b = a; return () => { b = 1 } } }`,
	`var f = function g() { return g }`,
	`class A { m() { return super.x } n() { return () => () => super.y } static s() { return () => new.target } }`,
	`class B extends Object { constructor() { var f = () => super(); f() } }`,
	`class C extends Object {}`,
	`class D extends Object { constructor(...a) { super(...a) } }`,
	`function F() { return () => new.target }`,
	`function F() { return () => () => () => eval("1") }`,
	`function* g() { yield 1; yield* [2] } function* h(a) { yield a } async function* i() { yield* j(); for await (const x of []) {} }`,
	`async function a(x) { await x; return () => arguments }`,
	`var o = { m() { return () => super.toString() }, get p() { return super.q }, set p(v) { super.q = v } }`,
	`function f(a, b = 1) { return arguments }`,
	`function f(a, {b}) { return () => b }`,
	`with (o) { (function (a) { return () => { a = 1 } }) }`,
}

// TestRecordedFactsMatchScans holds what the compiler records of each
// function's code to what the JIT and the VM had scanned the code for,
// for every function of factSources and, given QUICKJS_FACTS_CORPUS -- a
// list of directories, separated as PATH's are -- of every .js file under
// them, compiled in each of factModes: a script, strict, a module, for a
// debugger, a module for a debugger, with Node's quirks, and as eval code.
func TestRecordedFactsMatchScans(t *testing.T) {
	type job struct{ name, text string }
	var jobs []job
	for i, src := range factSources {
		jobs = append(jobs, job{fmt.Sprintf("source %d", i), src})
	}
	corpus := os.Getenv("QUICKJS_FACTS_CORPUS")
	var paths []string
	if corpus != "" {
		for _, root := range filepath.SplitList(corpus) {
			err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !d.IsDir() && strings.HasSuffix(p, ".js") {
					paths = append(paths, p)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	var (
		mu     sync.Mutex
		counts = make([]factCounts, len(factModes))
		diffs  []string
	)
	work := make(chan job)
	var wg sync.WaitGroup
	for range min(runtime.GOMAXPROCS(0), 6) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]factCounts, len(factModes))
			var found []string
			for j := range work {
				for m := range factModes {
					if d := compileFacts(j.name, j.text, m, &local[m]); d != "" && len(found) < 20 {
						found = append(found, d)
					}
				}
			}
			mu.Lock()
			for m := range local {
				counts[m].add(local[m])
			}
			diffs = append(diffs, found...)
			mu.Unlock()
		}()
	}
	for _, j := range jobs {
		work <- j
	}
	for _, p := range paths {
		text, err := os.ReadFile(p)
		if err != nil {
			t.Error(err)
			continue
		}
		work <- job{p, string(text)}
	}
	close(work)
	wg.Wait()
	for _, d := range diffs {
		t.Error(d)
	}
	var total factCounts
	for m, c := range counts {
		t.Logf("%-8s %v", factModes[m].name, c)
		total.add(c)
	}
	t.Logf("%-8s %v", "all", total)
	if corpus == "" {
		// Each fact holds of some function of factSources, and not of all.
		for _, c := range []int{total.frameCallee, total.readsMaker, total.assignsParams, total.makesArguments,
			total.forwards, total.yieldStar, total.argumentsOwn, total.assigned, total.closureAssigned, total.resumable} {
			if c == 0 || c == total.functions {
				t.Errorf("a fact true of %d of %d functions: %v", c, total.functions, total)
			}
		}
	}
}
