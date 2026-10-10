package vm

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// TestTreeExceptionCompletions pins that a function with handlers gives in
// the tree tier what it gives in the interpreter, and runs as a tree: catch
// and finally clauses entered by a throw from the function, from a getter
// and from a call; the completions a finally clause carries on -- a throw,
// a return, a break and a continue -- and those that replace them; nested
// clauses; closures made in a catch clause; and the frames a throwing
// nested tree leaves, which the handler's frame must not keep.
func TestTreeExceptionCompletions(t *testing.T) {
	defer SetTreeTier(true)
	for _, tt := range []struct {
		name, src, want string
	}{
		{"catch", `function f() { try { throw 7 } catch (e) { return e + 1 } } f()`, "8"},
		{"optionalBinding", `function f() { try { throw 7 } catch { return 8 } } f()`, "8"},
		{"identity", `function f() { var o = {}; try { throw o } catch (e) { return e === o } } f()`, "true"},
		{"undefined", `function f() { try { throw undefined } catch (e) { return e === undefined } } f()`, "true"},
		{"typeError", `function f() { try { return null.x } catch (e) { return e.constructor.name } } f()`, "TypeError"},
		{"referenceError", `function f() { try { return absentTreeName } catch (e) { return e.constructor.name } } f()`, "ReferenceError"},
		{"conversion", `function f() { try { return Symbol() + 1 } catch (e) { return e.constructor.name } } f()`, "TypeError"},
		{"getter", `var o = { get x() { throw 9 } }; function f() { try { return 1 + o.x } catch (e) { return e } } f()`, "9"},
		{"rethrow", `function f() { try { try { throw 1 } catch (e) { throw e + 1 } } catch (e) { return e + 1 } } f()`, "3"},
		{"normalFinally", `function f() { var s = ''; try { s += 't' } finally { s += 'f' } return s } f()`, "tf"},
		{"returnFinally", `var s = ''; function f() { try { return 7 } finally { s += 'f' } } f() + s`, "7f"},
		{"replaceReturn", `function f() { try { return 7 } finally { return 8 } } f()`, "8"},
		{"replaceThrow", `function f() { try { throw 7 } finally { return 8 } } f()`, "8"},
		{"throwFinally", `function f() { try { try { return 7 } finally { throw 8 } } catch (e) { return e } } f()`, "8"},
		{"nestedFinallyReturn", `var s = ''; function f() { try { try { return 7 } finally { s += 'i' } } finally { s += 'o' } } f() + s`, "7io"},
		{"nestedFinallyThrow", `var s = ''; function f() { try { try { try { throw 7 } finally { s += 'i' } } finally { s += 'o' } } catch (e) { return e } } f() + s`, "7io"},
		{"catchThrowsFinally", `var s = ''; function f() { try { try { throw 7 } catch (e) { s += 'c'; throw e + 1 } finally { s += 'f' } } catch (e) { return e } } f() + s`, "8cf"},
		{"returnSkipsCatch", `var s = ''; function f() { try { try { try { return 7 } finally { s += 'i' } } catch { s += 'c' } } finally { s += 'o' } } f() + s`, "7io"},
		{"savedThrowRecord", `var s = ''; function f() { try { try { throw 7 } finally { try { throw 2 } catch (e) { s += e } } } catch (e) { return e } } f() + s`, "72"},
		{"savedReturnRecord", `var s = ''; function f() { try { return 7 } finally { try { return 8 } finally { s += 'f' } } } f() + s`, "8f"},
		{"breakFinally", `function f() { var s = ''; for (var i = 0; i < 3; i++) { try { s += 't'; break } finally { s += 'f' } } return s } f()`, "tf"},
		{"continueFinally", `function f() { var s = ''; for (var i = 0; i < 3; i++) { try { s += i; continue } finally { s += 'f' } } return s } f()`, "0f1f2f"},
		{"breakInsideTry", `function f() { var s = ''; try { for (;;) { s += 't'; break } s += 'a' } finally { s += 'f' } return s } f()`, "taf"},
		{"finallyBreakOverridesReturn", `function f() { for (;;) { try { return 7 } finally { break } } return 8 } f()`, "8"},
		{"finallyContinueOverridesThrow", `function f() { var s = ''; for (var i = 0; i < 3; i++) { try { throw 7 } finally { s += i; continue } } return s } f()`, "012"},
		{"leaveCatchRegion", `function f() { var s = ''; try { for (;;) { try { break } catch { s += 'wrong' } } throw 7 } catch (e) { return s + e } } f()`, "7"},
		{"outerProtectedLoop", `function f() { var s = 0; try { for (var i = 0; i < 20; i++) s += i } catch { s = -1 } return s } f()`, "190"},
		{"repeatedCatch", `function f() { var s = 0; for (var i = 0; i < 20; i++) { try { if (i & 1) throw i; s++ } catch (e) { s += e } } return s } f()`, "110"},
		{"repeatedFinally", `function f() { var s = 0; for (var i = 0; i < 20; i++) { try { if (i & 1) continue; s += i } finally { s++ } } return s } f()`, "110"},
		{"catchClosures", `function f() { var a = []; for (var i = 0; i < 3; i++) { try { throw i } catch (e) { a.push(function () { return e }) } } var x = a[0], y = a[1], z = a[2]; return x() + ',' + y() + ',' + z() } f()`, "0,1,2"},
		{"nestedTreeFrames", `var saved; function g(n) { var x = n; saved = function () { return x }; if (n) return g(n - 1); throw 7 } function f() { var s = 0; for (var i = 0; i < 100; i++) { try { g(8) } catch (e) { s += e + saved() } } return s } f()`, "700"},
		{"nestedHandlerCalls", `function g(n) { try { if (n) return g(n - 1); throw 7 } catch (e) { if (n < 3) throw e + 1; return e } } function f() { var s = 0; for (var i = 0; i < 10; i++) s += g(8); return s } f()`, "100"},
		{"mixedTiers", `function g() { var x = +1; throw x + 6 } function f() { try { g() } catch (e) { return e } } f()`, "7"},
		// Throw statements that a handler of their own function catches,
		// which go on to it without a panic (throwToHandler): the innermost
		// handler, one left by a break, a throw from a catch or a finally
		// clause, and one that leaves the function.
		{"throwInnermost", `function f() { var s = ''; try { try { throw 'a' } catch (e) { s += 'in' + e } } catch (e) { s += 'out' + e } return s } f()`, "ina"},
		{"throwFromCatch", `function f() { var s = ''; try { try { throw 'a' } catch (e) { s += e; throw 'b' } } catch (e) { s += e } return s } f()`, "ab"},
		{"throwFromFinally", `function f() { var s = ''; try { try { s += 't' } finally { s += 'f'; throw 'x' } } catch (e) { s += e } return s } f()`, "tfx"},
		{"throwThroughFinally", `function f() { var s = ''; try { try { throw 'x' } finally { s += 'f' } } catch (e) { s += e } return s } f()`, "fx"},
		{"throwAfterBreak", `function f() { var s = ''; try { for (;;) { try { break } catch (e) { s += 'wrong' } } throw 'x' } catch (e) { s += e } return s } f()`, "x"},
		{"throwAfterBreakUncaught", `function f() { for (;;) { try { break } catch (e) { return 'wrong' } } throw 'x' } var r; try { f() } catch (e) { r = 'outside ' + e } r`, "outside x"},
		{"throwFromLoopInTry", `function f(n) { var s = 0; try { for (var i = 0; i < n; i++) { s += i; if (i == 7) throw s } } catch (e) { return 'c' + e } return s } f(10) + ',' + f(5)`, "c28,10"},
		{"throwInTryInLoop", `function f(n) { var s = ''; for (var i = 0; i < n; i++) { try { if (i % 3 == 0) throw i; s += '.' } catch (e) { s += e } } return s } f(10)`, "0..3..6..9"},
		{"throwOverridesInFinally", `function f() { try { try { throw 1 } finally { throw 2 } } catch (e) { return e } } f()`, "2"},
		{"throwOutOfCatch", `function f() { try { throw 'in' } catch (e) { throw 'out ' + e } } var r; try { f() } catch (e) { r = e } r`, "out in"},
		{"throwAfterLabeledContinue", `function f() { var s = ''; outer: for (var i = 0; i < 3; i++) { try { for (var j = 0; j < 3; j++) { if (j == 1) continue outer; if (i == 2) throw 'x' + i } } catch (e) { s += e } s += i } return s } f()`, "x22"},
		{"throwAfterCallThrew", `function g() { throw 'g' } function f() { var s = ''; try { g() } catch (e) { s += e; try { throw 'l' } catch (e2) { s += e2 } } return s } f()`, "gl"},
		{"finallyTailCall", `'use strict'; function f(n) { try { if (n === 0) return 7 } finally {} return f(n - 1) } f(1000)`, "7"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, on := range []bool{false, true} {
				SetTreeTier(on)
				fn := compileForTest(t, tt.src)
				r := New(Config{})
				v, err := r.Run(fn)
				if err != nil {
					r.Close()
					t.Fatalf("tree %v: %v", on, err)
				}
				got, err := r.ToString(v)
				if err != nil || got.Go() != tt.want {
					t.Errorf("tree %v: got %v, %v; want %s", on, got, err, tt.want)
				}
				if r.frameDepth != 0 || r.stackTop != 0 {
					t.Errorf("tree %v: left %d frames and %d stack slots", on, r.frameDepth, r.stackTop)
				}
				r.Close()
				if on {
					assertExceptionTree(t, fn, "f")
					if tt.name == "nestedTreeFrames" || tt.name == "nestedHandlerCalls" {
						assertExceptionTree(t, fn, "g")
					}
				}
			}
		})
	}
}

// assertExceptionTree fails t unless fn's function name ran as a tree.
func assertExceptionTree(t *testing.T, fn *bytecode.Function, name string) {
	t.Helper()
	for _, k := range fn.Constants {
		if k.Fn != nil && k.Fn.Name == name {
			tr := (*tree)(atomic.LoadPointer(&k.Fn.VMCode))
			if tr == nil || tr == noTree {
				_, why := TreeReport(k.Fn)
				t.Fatalf("%s did not run as a tree: %s", name, why)
			}
			return
		}
	}
	t.Fatalf("no function %s", name)
}

// TestTreeExceptionLoopStructures pins that a loop inside a try statement,
// or with one inside it, or in a catch clause, is structured whole: the
// handler's blocks, entered only by a throw, do not keep it from being.
func TestTreeExceptionLoopStructures(t *testing.T) {
	if !treeTier.Load() {
		t.Skip("the tree tier is off")
	}
	for _, src := range []string{
		`function f(n) { var s = 0; try { for (var i = 0; i < n; i++) s += i } catch { s = -1 } return s }`,
		`function f(n) { var s = 0; for (var i = 0; i < n; i++) { try { s += i } catch { s = -1 } } return s }`,
		`function f(n) { var s = 0; try { for (var i = 0; i < n; i++) s += i } finally { s++ } return s }`,
		`function f(n) { var s = 0; for (var i = 0; i < n; i++) { try { s += i } finally { s++ } } return s }`,
		`function f(n) { var s = 0; try { throw 1 } catch { for (var i = 0; i < n; i++) s += i } return s }`,
	} {
		seen := false
		flowSeen = func(f *flow, _ *tree) {
			seen = true
			for i, nd := range f.nodes {
				if !nd.dead && (nd.level >= 0 || has(nd.succ, i)) {
					t.Errorf("%s: loop block %d left to dispatch", src, i)
				}
			}
		}
		if buildTree(flowFunc(t, src)) == nil {
			t.Errorf("%s: not built", src)
		}
		flowSeen = nil
		if !seen {
			t.Errorf("%s: not structured", src)
		}
	}
}

// TestTreeExceptionInterrupt pins that an interrupt is not an exception a
// tree's handler catches: neither the catch nor the finally clause runs.
func TestTreeExceptionInterrupt(t *testing.T) {
	defer SetTreeTier(true)
	for _, on := range []bool{false, true} {
		SetTreeTier(on)
		r := New(Config{})
		ctx, cancel := context.WithCancel(context.Background())
		r.global.setOwnRaw(r.atoms.intern("cancelTree"), Obj(r.newNativeFunc("cancelTree", 0,
			func(*Runtime, Value, []Value) (Value, error) {
				cancel()
				return Undefined, nil
			})), propDefault)
		r.SetContext(ctx)
		fn := compileForTest(t, `var caught = false, finished = false; function f() { try { cancelTree(); for (;;) {} } catch { caught = true } finally { finished = true } } f()`)
		_, err := r.Run(fn)
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Errorf("tree %v: got %v, want cancellation", on, err)
		}
		r.SetContext(context.Background())
		v, err := r.Run(compileForTest(t, `caught + ',' + finished`))
		if err != nil || v.String().Go() != "false,false" {
			t.Errorf("tree %v: interrupt ran a handler: %v, %v", on, v, err)
		}
		r.Close()
		if on {
			assertExceptionTree(t, fn, "f")
		}
	}
}

// TestTreeExceptionSharedAcrossRuntimes runs one compiled function with
// handlers in runtimes on several goroutines at once, as a program shared
// between them is: the handlers are each frame's, not the tree's.
func TestTreeExceptionSharedAcrossRuntimes(t *testing.T) {
	if !treeTier.Load() {
		t.Skip("the tree tier is off")
	}
	fn := compileForTest(t, `function f() { var s = 0; for (var i = 0; i < 20; i++) { try { throw i } catch (e) { s += e } finally { s++ } } return s } f()`)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := New(Config{})
			defer r.Close()
			for range 10 {
				v, err := r.Run(fn)
				if err != nil || v.Number() != 210 {
					t.Errorf("got %v, %v; want 210", v, err)
				}
			}
		}()
	}
	wg.Wait()
	assertExceptionTree(t, fn, "f")
}

// TestTreeExceptionStackLimit pins that a call too deep, which throws a
// RangeError from inside nested trees, is caught by the handler that encloses
// it, leaving no frames and no stack behind.
func TestTreeExceptionStackLimit(t *testing.T) {
	defer SetTreeTier(true)
	for _, on := range []bool{false, true} {
		SetTreeTier(on)
		r := New(Config{MaxCallDepth: 24})
		fn := compileForTest(t, `function g(n) { return g(n + 1) } function f() { var s = 0; for (var i = 0; i < 20; i++) { try { g(0) } catch (e) { if (e.constructor.name === 'RangeError') s++ } } return s } f()`)
		v, err := r.Run(fn)
		if err != nil || v.Number() != 20 || r.frameDepth != 0 || r.stackTop != 0 {
			t.Errorf("tree %v: got %v, %v, depth %d, top %d", on, v, err, r.frameDepth, r.stackTop)
		}
		r.Close()
		if on {
			assertExceptionTree(t, fn, "f")
			assertExceptionTree(t, fn, "g")
		}
	}
}

// BenchmarkTreeExceptionNoThrow measures what a try statement that never
// throws costs a loop, inside it and around it, against the loop alone.
func BenchmarkTreeExceptionNoThrow(b *testing.B) {
	for _, tt := range []struct{ name, body string }{
		{"plain", `var s = 0; for (var i = 0; i < n; i++) s += i; return s`},
		{"catchOutsideLoop", `var s = 0; try { for (var i = 0; i < n; i++) s += i } catch { return -1 } return s`},
		{"finallyOutsideLoop", `var s = 0; try { for (var i = 0; i < n; i++) s += i } finally {} return s`},
		{"catchInsideLoop", `var s = 0; for (var i = 0; i < n; i++) { try { s += i } catch { return -1 } } return s`},
		{"finallyInsideLoop", `var s = 0; for (var i = 0; i < n; i++) { try { s += i } finally {} } return s`},
	} {
		b.Run(tt.name, func(b *testing.B) {
			src := "function f(n) { " + tt.body + " } f"
			p, err := parser.Parse(src, parser.Options{})
			if err != nil {
				b.Fatal(err)
			}
			fn, err := compiler.Compile(p, compiler.Options{Text: src})
			if err != nil {
				b.Fatal(err)
			}
			r := New(Config{})
			defer r.Close()
			f, err := r.Run(fn)
			if err != nil {
				b.Fatal(err)
			}
			args := []Value{Int32(8192)}
			if _, err := r.Call(f, Undefined, args); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				v, err := r.Call(f, Undefined, args)
				if err != nil || v.Number() != 33550336 {
					b.Fatalf("got %v, %v", v, err)
				}
			}
		})
	}
}
