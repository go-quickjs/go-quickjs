package quickjs_test

import "testing"

// A break or a continue leaves by jumping, so whatever the statements it is
// leaving put in place has to be undone at the jump: the operands they left on
// the stack, the iterators they opened, and the exception handlers they pushed.
// What it must not undo is anything that encloses where it is going.

// TestBreakOutOfTryCatch covers a jump that leaves a try block without passing
// through a catch or a finally. The handler the try pushed is still on the
// handler stack at the jump, and dropping it is the jump's job -- a handler
// left behind catches a throw that belongs to somebody else.
func TestBreakOutOfTryCatch(t *testing.T) {
	cases := []struct{ src, want string }{
		// The classic: the handler is finished with, and a later throw must
		// reach the caller rather than the catch clause it left.
		{`function f() {
		    for (;;) { try { break } catch (e) { return "caught by a dead handler" } }
		    throw new RangeError("late")
		  }
		  try { f() } catch (e) { e.constructor.name }`, "RangeError"},
		// The same for continue, which leaves the block just as abruptly.
		{`function f() {
		    var n = 0
		    for (var i = 0; i < 3; i++) { try { continue } catch (e) { return "wrong" } }
		    try { null.x } catch (e) { return "ok" }
		  }
		  f()`, "ok"},
		// Out of a labelled block rather than a loop.
		{`function f() {
		    out: { try { break out } catch (e) { return "wrong" } }
		    try { null.x } catch (e) { return "ok" }
		  }
		  f()`, "ok"},
		// Out through two try statements at once.
		{`function f() {
		    for (;;) { try { try { break } catch (e) {} } catch (e) {} }
		    try { null.x } catch (e) { return "ok" }
		  }
		  f()`, "ok"},
		// A handler that has not been left is still in force.
		{`function f() {
		    try { for (;;) { break }; null.x } catch (e) { return "still protected" }
		  }
		  f()`, "still protected"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestBreakInsideTryFinally covers the other half: a finally clause runs when
// the try block is left, and not when a jump stays inside it.
func TestBreakInsideTryFinally(t *testing.T) {
	cases := []struct{ src, want string }{
		// The loop is inside the try, so the break does not leave the try at
		// all: the clause runs once, when the block ends normally.
		{`var log = []
		  function f() { try { for (;;) { log.push("body"); break } } finally { log.push("fin") } }
		  f(); log.join(",")`, "body,fin"},
		// The same with continue, and with the loop running more than once.
		{`var log = []
		  function f() {
		    try { for (var i = 0; i < 3; i++) { continue } } finally { log.push("fin") }
		  }
		  f(); log.join(",")`, "fin"},
		// A break that does leave the try runs the clause on its way out, once.
		{`var log = []
		  function f() {
		    for (;;) { try { log.push("body"); break } finally { log.push("fin") } }
		    log.push("after")
		  }
		  f(); log.join(",")`, "body,fin,after"},
		// Two clauses are run innermost first.
		{`var log = []
		  function f() {
		    for (;;) {
		      try { try { break } finally { log.push("inner") } } finally { log.push("outer") }
		    }
		  }
		  f(); log.join(",")`, "inner,outer"},
		// A labelled break out of the whole statement runs the clause; the
		// loop inside it is left behind.
		{`var log = []
		  function f() {
		    out: try { for (;;) { log.push("body"); break out } } finally { log.push("fin") }
		    log.push("after")
		  }
		  f(); log.join(",")`, "body,fin,after"},
		// A clause between the jump and its target runs even though the target
		// is a loop further out.
		{`var log = []
		  function f() {
		    outer: for (;;) {
		      for (;;) { try { break outer } finally { log.push("fin") } }
		    }
		    log.push("after")
		  }
		  f(); log.join(",")`, "fin,after"},
		// A clause whose try block contains the target does not run early.
		{`var log = []
		  function f() {
		    try { outer: for (;;) { for (;;) { break outer } } log.push("still in try") }
		    finally { log.push("fin") }
		  }
		  f(); log.join(",")`, "still in try,fin"},
		// The function still returns what it was going to.
		{`function f() { try { for (;;) { break } } finally { } return 42 }
		  f()`, "42"},
		// And a throw after the block is not caught by the clause's handler.
		{`function f() {
		    try { for (;;) { break } } finally { }
		    throw new SyntaxError("after")
		  }
		  try { f() } catch (e) { e.constructor.name }`, "SyntaxError"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestUnwindInAsyncFunctions covers the same jumps where the frame can be
// suspended, since a corrupted stack there shows up only when it resumes.
func TestUnwindInAsyncFunctions(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var log = []
		  async function f() {
		    try { for (;;) { break } await null; log.push("resumed") } finally { log.push("fin") }
		    return "done"
		  }
		  f().then(v => log.push(v))
		  log.join(",")`, ""},
		{`var out = ""
		  async function f() {
		    try { for (;;) { break } await null; out += "resumed," } finally { out += "fin," }
		    return "done"
		  }
		  f().then(v => { out += v })
		  "started"`, "started"},
		// A generator's stack is the same stack.
		{`function* g() {
		    for (;;) { try { break } catch (e) { yield "wrong" } }
		    yield "after"
		  }
		  [...g()].join()`, "after"},
		{`var log = []
		  function* g() {
		    try { for (;;) { break } yield 1 } finally { log.push("fin") }
		  }
		  var seen = [...g()]
		  seen.join() + "|" + log.join()`, "1|fin"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}

// TestUnwindWithIterators covers a jump that leaves both a for-of and a try,
// where the iterator has to be closed and the handler dropped.
func TestUnwindWithIterators(t *testing.T) {
	cases := []struct{ src, want string }{
		{`var closed = false
		  var iterable = {[Symbol.iterator]() {
		    var i = 0
		    return {next: () => ({value: i++, done: false}),
		            return() { closed = true; return {done: true} }}
		  }}
		  function f() {
		    out: for (;;) {
		      for (const x of iterable) { try { break out } finally { } }
		    }
		    return closed
		  }
		  f()`, "true"},
		{`var log = []
		  function f() {
		    for (const x of [1, 2, 3]) {
		      try { if (x === 2) break; log.push(x) } finally { log.push("f" + x) }
		    }
		    try { null.x } catch (e) { log.push("protected") }
		    return log.join(",")
		  }
		  f()`, "1,f1,f2,protected"},
		// A switch leaves its discriminant on the stack. A break inside one
		// targets the switch itself, and has to drop the handler on its way.
		{`function f() {
		    switch (1) { case 1: try { break } catch (e) { return "wrong" } }
		    try { null.x } catch (e) { return "ok" }
		  }
		  f()`, "ok"},
		// A labelled break out of the loop around the switch drops the
		// discriminant as well as the handler.
		{`function f() {
		    out: for (;;) {
		      switch (1) { case 1: try { break out } catch (e) { return "wrong" } }
		    }
		    try { null.x } catch (e) { return "ok" }
		  }
		  f()`, "ok"},
		// And the same where the clause has to run on the way past.
		{`var log = []
		  function f() {
		    out: for (;;) {
		      switch (1) { case 1: try { break out } finally { log.push("fin") } }
		    }
		    return log.join()
		  }
		  f()`, "fin"},
	}
	for _, tc := range cases {
		checkEval(t, tc.src, tc.want)
	}
}
