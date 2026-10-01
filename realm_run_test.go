package quickjs_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// A call made from inside a running script whose own context ends stops that
// call alone: the host function gets an error wrapping the context's, and
// the script that called it runs on, as node:vm's timeout leaves the code
// that set it running. It used to stop the outer script too.
func TestNestedDeadlineStopsOnlyTheCall(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	spin, err := rt.Eval(`(function () { for (;;) {} })`)
	if err != nil {
		t.Fatal(err)
	}
	var evalErr, callErr error
	rt.Set("evalBriefly", func(r *quickjs.Runtime) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		_, evalErr = r.EvalContext(ctx, `for (;;) {}`)
	})
	rt.Set("callBriefly", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		_, callErr = spin.CallContext(ctx)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v, err := rt.EvalContext(ctx, `evalBriefly(); callBriefly(); "ran on"`)
	if err != nil {
		t.Fatalf("the outer script: %v", err)
	}
	if v.String() != "ran on" {
		t.Errorf("the outer script = %q, want %q", v.String(), "ran on")
	}
	for name, err := range map[string]error{"EvalContext": evalErr, "CallContext": callErr} {
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s inside = %v, want the deadline", name, err)
		}
	}

	// The outer script's own deadline still stops everything.
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	rt.Set("evalLong", func(r *quickjs.Runtime) error {
		_, err := r.EvalContext(context.Background(), `for (;;) {}`)
		return err
	})
	if _, err := rt.EvalContext(ctx, `evalLong(); "ran on"`); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the outer deadline = %v, want it to stop the script", err)
	}
}

// A realm made WithSandbox has the sandbox's properties for global names, and
// what its scripts declare globally lands on the sandbox.
func TestRealmWithSandbox(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	sandbox, err := rt.Eval(`({a: 1})`)
	if err != nil {
		t.Fatal(err)
	}
	re, err := rt.NewRealm(quickjs.WithSandbox(sandbox))
	if err != nil {
		t.Fatal(err)
	}
	p, err := quickjs.Compile("ctx.js", `var b = a + 1; typeof Array + "," + (globalThis.a === 1)`)
	if err != nil {
		t.Fatal(err)
	}
	v, err := re.RunProgram(p)
	if err != nil {
		t.Fatal(err)
	}
	if v.String() != "function,true" {
		t.Errorf("in the context = %q, want %q", v.String(), "function,true")
	}
	if b, _ := sandbox.Get("b"); b.Int() != 2 {
		t.Errorf("sandbox.b = %v, want 2", b)
	}
	if _, err := rt.NewRealm(quickjs.WithSandbox(quickjs.Value{})); err == nil {
		t.Error("a realm took a sandbox that is no object")
	}
}

// WithOffset places source within a larger file: its stack traces count the
// lines before it.
func TestCompileWithOffset(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	p, err := quickjs.Compile("page.html", "\n\nthrow new Error('x')", quickjs.WithOffset(10, 4))
	if err != nil {
		t.Fatal(err)
	}
	_, err = rt.RunProgram(p)
	var jsErr *quickjs.Error
	if !errors.As(err, &jsErr) {
		t.Fatalf("= %v, want the exception", err)
	}
	if stack := jsErr.Stack(); !strings.Contains(stack, "page.html:13:") {
		t.Errorf("stack = %q, want it at page.html:13", stack)
	}
}

// A runtime's Compile parses as the runtime does: WithNodeQuirks accepts what
// V8 accepts and the standard does not. The program it makes is refused by a
// standard runtime when it runs, as the package's Compile refuses it at once.
func TestRuntimeCompileQuirks(t *testing.T) {
	const src = `"use strict"; function f() {} if (false) f() = 1; "compiled"`
	quirks := quickjs.New(quickjs.WithNodeQuirks())
	defer quirks.Close()
	p, err := quirks.Compile("q.js", src)
	if err != nil {
		t.Fatalf("WithNodeQuirks: %v", err)
	}
	if v, err := quirks.RunProgram(p); err != nil || v.String() != "compiled" {
		t.Errorf("run WithNodeQuirks = %v, %v", v, err)
	}
	var syn *quickjs.SyntaxError
	if _, err := quickjs.Compile("q.js", src); !errors.As(err, &syn) {
		t.Errorf("the package's Compile = %v, want a SyntaxError", err)
	}
	standard := quickjs.New()
	defer standard.Close()
	if _, err := standard.RunProgram(p); !errors.As(err, &syn) {
		t.Errorf("a standard runtime running it = %v, want a SyntaxError", err)
	}
	if !standard.CodeGenerationAllowed() || quickjs.New(quickjs.WithoutCodeGeneration()).CodeGenerationAllowed() {
		t.Error("CodeGenerationAllowed does not say how the runtime was made")
	}
}
