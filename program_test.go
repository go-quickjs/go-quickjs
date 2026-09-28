package quickjs_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// programSrc touches what a runtime makes of compiled code for itself: its
// globals, closures, a class, a tagged template's cached strings, a regexp
// literal, and a promise.
const programSrc = `
var runs = (globalThis.runs || 0) + 1;
function counter() { let n = 0; return () => ++n }
var out;
{
	// A top-level class or const could not be declared again by the next run.
	class Point { constructor(x, y) { this.x = x; this.y = y } get norm() { return Math.hypot(this.x, this.y) } }
	const tag = s => s;
	const site = () => tag` + "`a${1}b`" + `;
	const sameSite = site() === site();
	Promise.resolve(runs).then(v => { out = [v, counter()(), new Point(3, 4).norm, sameSite, /b+/g.test("abb")].join() });
}
"completed " + runs`

// TestProgramRunsOnManyRuntimes pins that a program compiled once runs on
// any number of runtimes, several at once, each with values of its own, and
// more than once on one -- where, as for Eval, a top-level class or const
// declared again is a SyntaxError.
func TestProgramRunsOnManyRuntimes(t *testing.T) {
	p, err := quickjs.Compile("shared.js", programSrc)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rt := quickjs.New()
			defer rt.Close()
			for run := 1; run <= 3; run++ {
				v, err := rt.RunProgram(p)
				if err != nil {
					errs <- err
					return
				}
				if want := fmt.Sprintf("completed %d", run); v.String() != want {
					errs <- fmt.Errorf("completion %q, want %q", v, want)
					return
				}
				out, _ := rt.Get("out")
				if want := fmt.Sprintf("%d,1,5,true,true", run); out.String() != want {
					errs <- fmt.Errorf("out %q, want %q", out, want)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestCompile pins Compile's options and errors: WithStrict makes the whole
// script strict, a syntax error is a *SyntaxError from Compile, the name is
// what stack traces call the script, and RunProgramContext stops a program
// whose context ends.
func TestCompile(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	sloppy, err := quickjs.Compile("sloppy.js", `undeclared = 1; typeof undeclared`)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := rt.RunProgram(sloppy); err != nil || v.String() != "number" {
		t.Errorf("sloppy = %v, %v", v, err)
	}
	strict, err := quickjs.Compile("strict.js", `undeclaredToo = 1`, quickjs.WithStrict())
	if err != nil {
		t.Fatal(err)
	}
	var jsErr *quickjs.Error
	if _, err := rt.RunProgram(strict); !errors.As(err, &jsErr) || !strings.HasPrefix(err.Error(), "ReferenceError") {
		t.Errorf("strict = %T %v, want a ReferenceError", err, err)
	}
	if _, err := quickjs.Compile("with.js", `with ({}) {}`, quickjs.WithStrict()); err == nil {
		t.Error("with in strict code compiled")
	}

	var synErr *quickjs.SyntaxError
	if _, err := quickjs.Compile("bad.js", `1 +`); !errors.As(err, &synErr) {
		t.Errorf("bad = %T %v, want a *SyntaxError", err, err)
	}

	named, err := quickjs.Compile("app.js", "function main() {\n  return new Error('x').stack\n}\nmain()")
	if err != nil {
		t.Fatal(err)
	}
	if v, err := rt.RunProgram(named); err != nil || !strings.Contains(v.String(), "at main (app.js:2:10)") {
		t.Errorf("stack = %v, %v", v, err)
	}

	spin, err := quickjs.Compile("spin.js", `for (;;) {}`)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := rt.RunProgramContext(ctx, spin); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("spin = %v, want the deadline", err)
	}
	if v, err := rt.Eval(`1 + 1`); err != nil || v.Int() != 2 {
		t.Errorf("after = %v, %v", v, err)
	}

	if _, err := rt.RunProgram(nil); err == nil {
		t.Error("a nil program ran")
	}
	rt.Close()
	if _, err := rt.RunProgram(sloppy); !errors.Is(err, quickjs.ErrClosed) {
		t.Errorf("after Close = %v, want ErrClosed", err)
	}
}

// TestProgramNodeQuirks pins that a runtime WithNodeQuirks runs a program as
// it would compile the source itself: Annex B hoists a block-level function
// named arguments over the arguments object there, and not in standards mode.
func TestProgramNodeQuirks(t *testing.T) {
	p, err := quickjs.Compile("quirk.js",
		`(function (x) { var a = typeof arguments; { function arguments() {} } return a + "," + typeof arguments; })(1)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		opts []quickjs.Option
		want string
	}{
		{nil, "object,object"},
		{[]quickjs.Option{quickjs.WithNodeQuirks()}, "object,function"},
		{[]quickjs.Option{quickjs.WithNodeQuirks()}, "object,function"},
	} {
		rt := quickjs.New(c.opts...)
		if v, err := rt.RunProgram(p); err != nil || v.String() != c.want {
			t.Errorf("quirks=%v: %v, %v, want %s", len(c.opts) > 0, v, err, c.want)
		}
		rt.Close()
	}
}
