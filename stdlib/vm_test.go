package stdlib_test

import (
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/stdlib"
)

// vmLines runs a script that imports node:vm as vm and logs a line a check,
// and returns what it logged. Each answer is node's.
func vmLines(t *testing.T, body string) []string {
	t.Helper()
	out, errOut := run(t, stdlib.Config{}, `import("node:vm").then(({default: vm}) => {`+body+`}).catch(e => console.error(e && e.stack || e))`)
	if errOut != "" {
		t.Fatalf("stderr: %s", errOut)
	}
	return strings.Split(out, "\n")
}

func checkLines(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestVMContextify pins what a context's global names are: the sandbox's
// properties, then the context's own built-ins; function declarations and
// assigned names land on the sandbox, and let, const and class stay in the
// context. Node lists v before fn: V8 creates globals in the order they are
// written, which WithNodeQuirks reproduces, and the standard creates the
// functions first.
func TestVMContextify(t *testing.T) {
	checkLines(t, vmLines(t, `
		const sb = { a: 1 };
		const ctx = vm.createContext(sb);
		console.log(ctx === sb, vm.isContext(sb), vm.isContext({}), vm.runInContext("this", ctx) === sb);
		vm.runInContext("var v = 1; function fn() { return 2 } w = 3; let l = 4; const c = 5; class K {}", ctx);
		console.log(JSON.stringify(Object.keys(sb)), typeof sb.fn, sb.l, sb.K, vm.runInContext("l + c + typeof K", ctx));
		console.log(vm.runInContext("a + 1", ctx), vm.runInContext("this.a + globalThis.v", ctx));
		sb.late = 10;
		console.log(vm.runInContext("late", ctx), vm.runInContext("Object.keys(this).join()", ctx));
		console.log(vm.runInContext("typeof Array + ',' + (Array === this.Array) + ',' + ('Array' in this)", ctx), "Array" in sb);
		vm.runInContext("Array = 5", ctx);
		console.log(sb.Array, vm.runInContext("Array", ctx));
		console.log(vm.runInContext("delete Array", ctx), "Array" in sb, vm.runInContext("typeof Array", ctx));
		console.log(vm.runInContext("undefined = 1; typeof undefined", ctx), "undefined" in sb);
		const g = vm.createContext(Object.defineProperty(Object.create({ inherited: 7 }), "got", { get() { return this === g } }));
		console.log(vm.runInContext("inherited + ',' + got", g));
	`),
		"true true false false",
		`["a","fn","v","w"] function undefined undefined 9function`,
		"2 2",
		"10 fn,v,a,w,late",
		"function,true,true false",
		"5 5",
		"true false undefined",
		"undefined false",
		"7,true",
	)
}

// TestVMRealms pins that a context is a realm of its own: its built-ins, its
// errors and its functions are its own, and objects pass in and out as they
// are.
func TestVMRealms(t *testing.T) {
	checkLines(t, vmLines(t, `
		const shared = [1];
		const ctx = vm.createContext({ shared });
		console.log(vm.runInContext("[]", ctx) instanceof Array, Array.isArray(vm.runInContext("[]", ctx)),
			vm.runInContext("shared instanceof Array", ctx), vm.runInContext("shared", ctx) === shared);
		try { vm.runInContext("throw new TypeError('t')", ctx) } catch (e) { console.log(e instanceof TypeError, e.name) }
		try { vm.runInContext("nope", ctx) } catch (e) { console.log(e instanceof ReferenceError, e.name) }
		console.log(vm.runInContext("(function () { return this })()", ctx) === ctx);
		console.log(vm.runInContext("eval('var ev = 9'); ev", ctx), ctx.ev);
		console.log(vm.runInNewContext("x * 2", { x: 21 }), vm.runInNewContext("typeof console"));
		const plain = vm.createContext(vm.constants.DONT_CONTEXTIFY);
		console.log(vm.isContext(plain), vm.runInContext("globalThis", plain) === plain, plain.Array === Array);
	`),
		"false true false true",
		"false TypeError",
		"false ReferenceError",
		"false",
		"9 9",
		"42 object",
		"true true false",
	)
}

// TestVMScript pins Script and the functions over it: compiling once and
// running in several contexts, the file name and offsets a trace reports, a
// SyntaxError at construction, and a timeout the caller can catch.
func TestVMScript(t *testing.T) {
	checkLines(t, vmLines(t, `
		const s = new vm.Script("count = (typeof count === 'number' ? count : 0) + 1", { filename: "s.js" });
		const c = vm.createContext({});
		s.runInContext(c); s.runInContext(c);
		console.log(c.count, s.runInNewContext({ count: 5 }));
		console.log(vm.runInThisContext("var ritc = 1; typeof ritc"), typeof globalThis.ritc);
		console.log(vm.runInContext("new Error('e').stack.split('\\n')[1]", c, { filename: "file.js" }));
		console.log(vm.runInContext("new Error('e').stack.split('\\n')[1]", c, { filename: "f.js", lineOffset: 10, columnOffset: 5 }));
		try { new vm.Script("(", { filename: "bad.js" }) } catch (e) { console.log(e instanceof SyntaxError) }
		try { vm.runInContext("while (true) {}", vm.createContext({}), { timeout: 50 }) } catch (e) { console.log(e.name, e.code, e.message) }
		console.log(vm.runInContext("1", c, { timeout: 1000 }));
		try { vm.runInContext("1", {}) } catch (e) { console.log(e.name, e.code) }
		try { vm.isContext(1) } catch (e) { console.log(e.name, e.code) }
		try { vm.runInContext("1", c, { timeout: -1 }) } catch (e) { console.log(e.name, e.code) }
		console.log(Object.getOwnPropertyNames(vm.Script.prototype).sort().join());
		console.log(JSON.stringify(Object.keys(vm.constants)));
	`),
		"2 6",
		"number number",
		"    at file.js:1:1",
		"    at f.js:11:6",
		"true",
		"Error ERR_SCRIPT_EXECUTION_TIMEOUT Script execution timed out after 50ms",
		"1",
		"TypeError ERR_INVALID_ARG_TYPE",
		"TypeError ERR_INVALID_ARG_TYPE",
		"RangeError ERR_OUT_OF_RANGE",
		"constructor,runInContext,runInNewContext,runInThisContext",
		`["USE_MAIN_CONTEXT_DEFAULT_LOADER","DONT_CONTEXTIFY"]`,
	)
}

// TestVMCompileFunction pins compileFunction: a function of the given
// parameters whose free names are looked up in the context extensions first.
func TestVMCompileFunction(t *testing.T) {
	checkLines(t, vmLines(t, `
		const f = vm.compileFunction("return a + b + x", ["a", "b"], { contextExtensions: [{ x: 100 }] });
		console.log(f(1, 2), JSON.stringify(f.name), f.length);
		const g = vm.compileFunction("return typeof y", [], { parsingContext: vm.createContext({ y: 1 }) });
		console.log(g());
	`),
		`103 "" 2`,
		"number",
	)
}

// TestVMRespectsNoCodeGeneration pins that node:vm compiles nothing in a
// runtime made without code generation, as eval compiles nothing there.
func TestVMRespectsNoCodeGeneration(t *testing.T) {
	rt := quickjs.New(quickjs.WithoutCodeGeneration())
	defer rt.Close()
	if err := stdlib.VM(rt); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.EvalModule("main.js", `
		import vm from "node:vm";
		let r;
		try { vm.runInNewContext("1") } catch (e) { r = e.name }
		globalThis.result = r;
	`); err != nil {
		t.Fatal(err)
	}
	v, err := rt.Eval("result")
	if err != nil || v.String() != "EvalError" {
		t.Errorf("result = %v, %v", v, err)
	}
}
