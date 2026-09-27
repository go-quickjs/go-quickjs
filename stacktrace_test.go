package quickjs_test

import (
	"errors"
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// frames returns an error's frames, joined on one line. Each case's answer
// is V8's, from Node's vm.runInNewContext with the file named <eval>.
const frames = `function frames(e) { return e.stack.split("\n").slice(1).map(s => s.trim()).join(" | ") }
`

// TestStackTraceFormat pins the stack V8 writes: a header, and a frame a line
// naming the function as a method, a constructor or on its own, with where it
// is to the column.
func TestStackTraceFormat(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		// The constructor's own frame is left out; top-level code has no name.
		{frames + `function f() { return new Error("x") }
f().stack`, "Error: x\n    at f (<eval>:2:23)\n    at <eval>:3:1"},
		{frames + `function Foo() { this.e = new Error() }
frames(new Foo().e)`, "at new Foo (<eval>:2:27) | at <eval>:3:8"},
		{frames + `var o = { m() { return new Error() } }
frames(o.m())`, "at Object.m (<eval>:2:24) | at <eval>:3:10"},
		{frames + `class K { static s() { return new Error() } m() { return new Error() } }
frames(K.s()) + " / " + frames(new K().m())`,
			"at K.s (<eval>:2:31) | at <eval>:3:10 / at K.m (<eval>:2:58) | at <eval>:3:40"},
		{frames + `var o = { alias: function real() { return new Error() } }
frames(o.alias())`, "at Object.real [as alias] (<eval>:2:43) | at <eval>:3:10"},
		// A built-in is named for its receiver's type and has no location.
		{frames + `var e; try { [1].map(() => { throw new Error() }) } catch (x) { e = x }
frames(e)`, "at <eval>:2:36 | at Array.map (<anonymous>) | at <eval>:2:18"},
		// An error the engine throws is placed at the property it failed on.
		{frames + `var e; try { null.x } catch (x) { e = x }
e.stack`, "TypeError: cannot read property \"x\" of null\n    at <eval>:2:19"},
		// A call is placed at its name, or at its parenthesis when it has none.
		{frames + `frames((function () { return (() => new Error())() })())`, "at <eval>:2:37 | at <eval>:2:49 | at <eval>:2:54"},
		// Function.prototype.call leaves no frame of its own.
		{frames + `function L() { return Error.call(this) }
frames(new L())`, "at new L (<eval>:2:29) | at <eval>:3:8"},
		// Eval code says where the eval was, and a Function-made function is
		// eval too.
		{frames + `frames(eval("new Error()"))`, "at eval (eval at <anonymous> (<eval>:2:8), <anonymous>:1:1) | at <eval>:2:8"},
		{frames + `function g() { return eval("(function h() { return new Error() })()") }
frames(g())`, "at h (eval at g (<eval>:2:23), <anonymous>:1:24) | at eval (eval at g (<eval>:2:23), <anonymous>:1:38) | at g (<eval>:2:23) | at <eval>:3:8"},
		{frames + `frames(new Function("return new Error()")())`, "at eval (eval at <anonymous> (<eval>:2:8), <anonymous>:3:8) | at <eval>:2:42"},
		// A column counts UTF-16 code units, so an astral character is two.
		{frames + `var s = "😀"; var e = new Error()
frames(e)`, "at <eval>:2:23"},
	})
}

// TestStackTraceSubclass pins that a subclass's constructors are left out,
// and that the header is its name and message as they are when the stack is
// first read.
func TestStackTraceSubclass(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		{frames + `class AppError extends Error { constructor(m) { super(m); this.name = "AppError" } }
class Deeper extends AppError {}
function make() { return new Deeper("m") }
var e = make(); e.stack.split("\n")[0] + " | " + frames(e)`, "AppError: m | at make (<eval>:4:26) | at <eval>:5:9"},
		{`var e = new Error("a"); e.message = "b"; var first = e.stack; e.message = "c"; first + "," + e.stack`,
			"Error: b\n    at <eval>:1:9,Error: b\n    at <eval>:1:9"},
		{`var e = new Error("m"); Object.defineProperty(e, "name", { get() { return "Named" } }); e.stack.split("\n")[0]`, "Named: m"},
	})
}

// TestStackTraceAccessor pins that stack is an own accessor every error
// shares, as V8 has it, and that setting it replaces what it reads.
func TestStackTraceAccessor(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		{`var a = Object.getOwnPropertyDescriptor(new Error(), "stack"), b = Object.getOwnPropertyDescriptor(new TypeError(), "stack");
[typeof a.get, typeof a.set, a.get === b.get, a.set === b.set, a.enumerable, a.configurable, a.get.name, a.get.length, a.set.length,
 Object.prototype.hasOwnProperty.call(Error.prototype, "stack")].join()`, "function,function,true,true,false,true,,0,1,false"},
		{`var e = new Error("x"); e.stack = 42; [e.stack, typeof Object.getOwnPropertyDescriptor(e, "stack").get].join()`, "42,function"},
		{`var get = Object.getOwnPropertyDescriptor(new Error(), "stack").get; [get.call({}), get.call(1)].join()`, ","},
		{`var e = new Error(); delete e.stack; [e.stack, "stack" in e].join()`, ",false"},
		{`JSON.stringify(Object.keys(new Error("x")))`, "[]"},
	})
}

// TestStackTraceLimit pins Error.stackTraceLimit: how many frames are kept,
// none for zero or less, and no stack at all when it is not a number.
func TestStackTraceLimit(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		{`JSON.stringify(Object.getOwnPropertyDescriptor(Error, "stackTraceLimit"))`,
			`{"value":10,"writable":true,"enumerable":true,"configurable":true}`},
		{`function deep(n) { return n ? deep(n - 1) : new Error() } String(deep(30).stack.split("\n").length - 1)`, "10"},
		{`Error.stackTraceLimit = 2.9; function deep(n) { return n ? deep(n - 1) : new Error() } String(deep(30).stack.split("\n").length - 1)`, "2"},
		{`Error.stackTraceLimit = -1; new Error("z").stack`, "Error: z"},
		{`Error.stackTraceLimit = "3"; var e = new Error(); [typeof e.stack, "stack" in e].join()`, "undefined,true"},
		{`delete Error.stackTraceLimit; typeof new Error().stack`, "undefined"},
		{`Error.stackTraceLimit = Infinity; function deep(n) { return n ? deep(n - 1) : new Error() } String(deep(30).stack.split("\n").length - 1)`, "32"},
	})
}

// TestPrepareStackTrace pins Error.prepareStackTrace: asked for the stack when
// it is first read, with the error and its frames, and its answer kept.
func TestPrepareStackTrace(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		{`var args, self; Error.prepareStackTrace = function (e, cs) { args = [e, cs]; self = this; return { n: cs.length } };
var e = new Error(); var s = e.stack;
[typeof s, args[0] === e, Array.isArray(args[1]), self === Error, s === e.stack, Object.isFrozen(args[1])].join()`,
			"object,true,true,true,true,false"},
		// It is consulted when the stack is read, not when the error is made.
		{`var e = new Error(); Error.prepareStackTrace = () => "late"; e.stack`, "late"},
		// What it throws is thrown by the read, and nothing is kept.
		{`Error.prepareStackTrace = () => { throw new RangeError("boom") }; var e = new Error(), r = [];
try { e.stack } catch (x) { r.push(x.name) } Error.prepareStackTrace = undefined; r.push(e.stack.split("\n")[0]); r.join()`,
			"RangeError,Error"},
		// An error made while it runs gets an ordinary stack.
		{`Error.prepareStackTrace = () => typeof new Error("inner").stack; new Error().stack`, "string"},
	})
}

// TestCallSite pins what a CallSite says about its frame.
func TestCallSite(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		{`Error.prepareStackTrace = (e, cs) => cs;
function Bar() { this.s = new Error().stack[0] }
var c = new Bar().s;
[c.getFunctionName(), c.getMethodName(), c.getTypeName(), c.isToplevel(), c.isConstructor(), c.isNative(), c.isEval(),
 c.getFileName(), c.getLineNumber(), c.getColumnNumber(), c.getEnclosingLineNumber(), c.getEnclosingColumnNumber(), c.getPosition(),
 typeof c.getThis(), c.getFunction() === Bar, c.getEvalOrigin(), c.isAsync(), c.isPromiseAll(), c.getPromiseIndex(), String(c)].join("/")`,
			"Bar///false/true/false/false/<eval>/2/27/2/1/67/object/true//false/false//new Bar (<eval>:2:27)"},
		{`Error.prepareStackTrace = (e, cs) => cs;
var o = { go() { return new Error().stack[0] } }; var c = o.go();
[c.getFunctionName(), c.getMethodName(), c.getTypeName(), c.isToplevel(), String(c)].join("/")`,
			"go/go/Object/false/Object.go (<eval>:2:25)"},
		// A strict frame, and a built-in's, keep their receiver and function
		// to themselves.
		{`Error.prepareStackTrace = (e, cs) => cs;
var c = (function () { "use strict"; return new Error().stack[0] })();
[c.isToplevel(), typeof c.getThis(), typeof c.getFunction(), String(c)].join("/")`, "true/undefined/undefined/<eval>:2:45"},
		{`Error.prepareStackTrace = (e, cs) => cs; var c;
try { [1].forEach(() => { throw new Error() }) } catch (e) { c = e.stack[1] }
[c.getFunctionName(), c.getTypeName(), c.getFileName(), c.getLineNumber(), typeof c.getThis(), String(c)].join("/")`,
			"forEach/Array///undefined/Array.forEach (<anonymous>)"},
		{`Error.prepareStackTrace = (e, cs) => cs; var c = eval("new Error().stack[0]");
[c.getFunctionName(), c.isEval(), c.getFileName(), c.getEvalOrigin()].join("/")`, "eval/true//eval at <anonymous> (<eval>:1:50)"},
		{`Error.prepareStackTrace = (e, cs) => cs; var c = new Error().stack[0], proto = Object.getPrototypeOf(c), r = [];
r.push(Object.keys(c).length, proto.constructor.name, typeof globalThis.CallSite);
try { proto.getLineNumber.call({}) } catch (x) { r.push(x.message) }
try { new proto.constructor() } catch (x) { r.push(x.name) }
r.join()`, "0,CallSite,undefined,CallSite method getLineNumber expects CallSite as receiver,TypeError"},
	})
}

// TestCaptureStackTrace pins Error.captureStackTrace.
func TestCaptureStackTrace(t *testing.T) {
	evalCases(t, []struct{ src, want string }{
		{frames + `function f() { var o = {}; Error.captureStackTrace(o); return o }
var o = f(); [o.stack.split("\n")[0], frames(o), typeof Object.getOwnPropertyDescriptor(o, "stack").get].join(" / ")`,
			"Error / at f (<eval>:2:34) | at <eval>:3:9 / function"},
		{`var o = { name: "N", message: "M" }; Error.captureStackTrace(o); o.stack.split("\n")[0]`, "N: M"},
		// Frames up to and including the function given are left out.
		{frames + `function outer() { return inner() }
function inner() { var o = {}; Error.captureStackTrace(o, inner); return o }
frames(outer())`, "at outer (<eval>:2:27) | at <eval>:4:8"},
		{`function notCalled() {} var o = {}; Error.captureStackTrace(o, notCalled); JSON.stringify(o.stack)`, `"Error"`},
		// The usual way a subclass keeps its constructor out of its trace.
		{frames + `function MyError(m) { this.message = m; Error.captureStackTrace(this, MyError) }
function make() { return new MyError("m") }
frames(make())`, "at make (<eval>:3:26) | at <eval>:4:8"},
		{`var r = [];
for (var v of [1, "s", Object.freeze({}), Object.preventExtensions({}), new Proxy({}, {})]) {
	try { Error.captureStackTrace(v); r.push("ok") } catch (e) { r.push(e.message) }
}
var o = {}; Object.defineProperty(o, "stack", { value: 1 });
try { Error.captureStackTrace(o) } catch (e) { r.push(e.message) }
r.push(Error.captureStackTrace({}), Error.captureStackTrace.length);
r.join(" | ")`, "invalid_argument | invalid_argument | Cannot define property stack, object is not extensible | " +
			"Cannot define property stack, object is not extensible | invalid_argument | Cannot redefine property: stack |  | 2"},
		// Capturing again replaces the trace.
		{frames + `var e = new Error("x"); function again() { Error.captureStackTrace(e) } again(); frames(e)`,
			"at again (<eval>:2:50) | at <eval>:2:73"},
	})
}

// TestThrownValueStackHasColumns pins the trace a Go host gets for a thrown
// value that is not an error, which the engine records itself.
func TestThrownValueStackHasColumns(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	_, err := rt.EvalFile("main.js", "function f() {\n  throw 'plain'\n}\nf()")
	var jsErr *quickjs.Error
	if err == nil || !errors.As(err, &jsErr) {
		t.Fatalf("err = %v, want a *quickjs.Error", err)
	}
	if got, want := jsErr.Stack(), "at f (main.js:2:3)"; !strings.Contains(got, want) {
		t.Errorf("Stack() = %q, want it to contain %q", got, want)
	}
}
