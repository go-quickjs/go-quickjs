package compile

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// ssaRefusal is what the new pipeline makes of fn, as
// TestLowerCorpusRefusals counts it: "lowered" for Go's entry (LowerSSA);
// for a function Go's entry leaves for doing no loop's or array's work,
// "lowered callee" or "callee: <reason>" as native callers and inlining
// lower it (LowerSSAInline), or, a generator's or an async function's,
// "lowered hot" or "hot: <reason>" as LowerSSAHot does once its
// resumptions have made it hot; and "entry: <reason>" for one Go's entry
// refuses outright.
func ssaRefusal(fn *bytecode.Function) string {
	reason := func(err error) string {
		var rf *Refusal
		if errors.As(err, &rf) {
			return rf.Reason
		}
		return "error: " + err.Error()
	}
	_, err := LowerSSA(fn)
	switch {
	case err == nil:
		return "lowered"
	case HostOnly(err) && (fn.Async || fn.Generator):
		if _, err := LowerSSAHot(fn); err != nil {
			return "hot: " + reason(err)
		}
		return "lowered hot"
	case HostOnly(err):
		if _, err := LowerSSAInline(fn); err != nil {
			return "callee: " + reason(err)
		}
		return "lowered callee"
	}
	return "entry: " + reason(err)
}

// knownRefusals are the new pipeline's refusals of real code: each form
// TestLowerCorpusRefusals finds refused in npm's code, written small, with
// how many of the 231,941 functions of 60 popular packages and their
// dependencies (lodash, babel, typescript, prettier, sass, react, mathjs,
// graphql and the rest, 2026-11-08) were refused for it, 954 in all.
//
// The list only shrinks: a change that lowers one of them fails
// TestKnownSSARefusals until it is taken off, and one that refuses
// something new adds it here with its count, if it must.
var knownRefusals = []struct {
	name, source, want string
}{
	// 682 for an arguments object, all three forms. A sloppy function's
	// mapped object aliases its parameters, which native code holds as
	// values, so it is Go's only where neither is written nor the object
	// let out (argumentsOwn).
	{"mapped arguments, a parameter assigned",
		`function f(a) { a = 2; let s = 0; for (let i = 0; i < 3; i++) s += arguments[0]; return s }`,
		"entry: arguments object"},
	{"mapped arguments let out",
		`function f(a) { let s = 0; for (let i = 0; i < 3; i++) s += g(arguments); return s }`,
		"entry: arguments object"},
	// A forwarding function is its callers' to inline, its arguments
	// forwarded (LowerSSAForward); its own code would leave at each.
	{"arguments forwarded",
		`function f() { return g.apply(this, arguments) }`,
		"entry: arguments object"},
	// 246: locals, operand stack and upvalues past ir.MaxSlots together.
	{"slot budget", manyLocals(300), "entry: slot budget"},
	// 9.
	{"for await",
		`async function f(x) { let s = 0; for await (const v of x) s += v; return s }`,
		"entry: unsupported opcode for_await_of_start"},
	// 6, wrappers that construct a class for their caller.
	{"new.target",
		`function F(...args) { return Reflect.construct(C, args, new.target && new.target !== F ? new.target : C) }`,
		"entry: unsupported opcode new_target"},
	// 7, among them yaml's event builders: a parameter list of defaults
	// whose prologue's states have no room to keep the call's arguments
	// past their operands (prologueArgs).
	{"long list of defaults in a callee",
		`function f(s, a, b, c, d, e, g, h, i = 1, j = -1, k = !1) { s.push({a, b, c, d, e, g, h, i, j, k}) }`,
		"callee: non-simple parameters"},
	// 2: a class's or a literal's accessor of a computed key -- `get
	// [Symbol.toStringTag]()` in graphql's and postcss's modules.
	{"computed getter",
		`function f(k) { return { get [k]() { return 1 } } }`,
		"callee: unsupported opcode define_getter_index"},
	// None in npm's code, the getter's twin.
	{"computed setter",
		`function f(k) { return { set [k](v) {} } }`,
		"callee: unsupported opcode define_setter_index"},
	// 1: a class of private methods made by its module's code, as
	// lru-cache's is.
	{"private method",
		`function f() { return class { #m() { return 1 } } }`,
		"callee: unsupported opcode add_private_method"},
	// 1: f.apply's arguments a copy of the arguments object.
	{"apply of arguments",
		`function f() { var args = arguments.length === 1 ? [arguments[0]] : Array.apply(null, arguments); return args }`,
		"callee: unsupported opcode apply_arguments"},
}

// manyLocals is a function of n let bindings and a loop over two of them.
func manyLocals(n int) string {
	var b strings.Builder
	b.WriteString("function f() {")
	for i := range n {
		fmt.Fprintf(&b, " let v%d = %d;", i, i)
	}
	fmt.Fprintf(&b, " let s = 0; for (let i = 0; i < 3; i++) s += v0 + v%d; return s }", n-1)
	return b.String()
}

// TestKnownSSARefusals pins each of knownRefusals to its refusal, so that
// what the JIT still refuses of real code is a list a change shortens, and
// a refusal that changes its reason is seen.
func TestKnownSSARefusals(t *testing.T) {
	for _, c := range knownRefusals {
		t.Run(c.name, func(t *testing.T) {
			got := ssaRefusal(compiledFunction(t, c.source))
			switch {
			case got == c.want:
			case strings.HasPrefix(got, "lowered"):
				t.Fatalf("%s now, no longer refused: take it off knownRefusals", got)
			default:
				t.Fatalf("refusal %q, want %q", got, c.want)
			}
		})
	}
}
