package quickjs_test

import (
	"context"
	"errors"
	"iter"
	"runtime"
	"strings"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// naturals is 0, 1, 2, ... and records what it gave and whether it was
// stopped, which a sequence script stops early must be.
type naturals struct {
	given   []int
	stopped bool
}

func (n *naturals) seq() iter.Seq[int] {
	return func(yield func(int) bool) {
		defer func() { n.stopped = true }()
		for i := 0; ; i++ {
			n.given = append(n.given, i)
			if !yield(i) {
				return
			}
		}
	}
}

// TestGoSequences covers an iter.Seq and an iter.Seq2 handed to script:
// iterated by for-of, spread, destructuring and the iterator helpers, pulled
// one value at a time, and stopped when script stops early -- by break,
// return, a destructuring or a helper that takes fewer -- as a Go range over
// it would stop it.
func TestGoSequences(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	var nat *naturals
	rt.Set("nat", func() iter.Seq[int] { nat = &naturals{}; return nat.seq() })
	rt.Set("countdown", func(n int) iter.Seq[int] {
		return func(yield func(int) bool) {
			for i := n; i > 0; i-- {
				if !yield(i) {
					return
				}
			}
		}
	})
	rt.Set("pairs", func() iter.Seq2[string, int] {
		return func(yield func(string, int) bool) {
			_ = yield("a", 1) && yield("b", 2)
		}
	})
	rt.Set("noSeq", func() iter.Seq[int] { return nil })
	rt.Set("badSeq", func() iter.Seq[chan int] {
		return func(yield func(chan int) bool) { yield(make(chan int)) }
	})

	cases := []struct {
		src, want string
		given     string // what nat gave, for a case that uses it
	}{
		{`[...countdown(3)].join()`, "3,2,1", ""},
		{`var s = 0; for (const x of countdown(4)) s += x; s`, "10", ""},
		{`var r = []; for (const x of nat()) { if (x === 2) break; r.push(x) } r.join()`, "0,1", "0,1,2"},
		{`var [a, b] = nat(); a + "," + b`, "0,1", "0,1"},
		{`nat().map(x => x * 10).take(3).toArray().join()`, "0,10,20", "0,1,2"},
		{`var r = []; for (const [k, v] of pairs()) r.push(k + "=" + v); r.join()`, "a=1,b=2", ""},
		{`JSON.stringify([...pairs()])`, `[["a",1],["b",2]]`, ""},
		{`var it = countdown(2); [it.next().value, it.next().value, it.next().done, it.next().done].join()`, "2,1,true,true", ""},
		{`var it = nat(); it.next(); JSON.stringify([it.return(5), it.next()])`,
			`[{"value":5,"done":true},{"done":true}]`, "0"},
		{`var it = countdown(1); [it[Symbol.iterator]() === it,
		   Object.getPrototypeOf(Object.getPrototypeOf(it)) === Iterator.prototype,
		   typeof it.next, Object.prototype.toString.call(it)].join()`, "true,true,function,[object Iterator]", ""},
		{`noSeq() === null`, "true", ""},
		{`try { badSeq().next(); "no" } catch (e) { e.constructor.name }`, "TypeError", ""},
		{`try { Object.getPrototypeOf(countdown(1)).next.call({}); "no" } catch (e) { e.constructor.name }`, "TypeError", ""},
	}
	for _, tc := range cases {
		nat = nil
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
			continue
		}
		if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
		if tc.given != "" {
			var g []string
			for _, i := range nat.given {
				g = append(g, string(rune('0'+i)))
			}
			if got := strings.Join(g, ","); got != tc.given || !nat.stopped {
				t.Errorf("%s: the sequence gave %s and stopped=%v, want %s and stopped", tc.src, got, nat.stopped, tc.given)
			}
		}
	}
}

// TestGoSequenceReentered pins that script called from inside a sequence
// cannot ask the same iterator for a value while it is producing one, as a
// generator refuses to be resumed while it runs.
func TestGoSequenceReentered(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	rt.Set("calling", func(f quickjs.Value) iter.Seq[string] {
		return func(yield func(string) bool) {
			v, err := f.Call()
			if err != nil {
				yield("threw: " + err.Error())
				return
			}
			yield(v.String())
		}
	})
	v, err := rt.Eval(`var it = calling(() => it.next()); it.next().value`)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String(); !strings.Contains(got, "TypeError") || !strings.Contains(got, "already running") {
		t.Errorf("got %q, want the inner next refused with a TypeError", got)
	}
}

// TestGoSequenceStoppedWhenDropped pins that a sequence script stops using
// is stopped by the time the runtime closes, and one script drops is
// stopped once the collector finds its iterator.
func TestGoSequenceStoppedWhenDropped(t *testing.T) {
	t.Run("close", func(t *testing.T) {
		rt := quickjs.New()
		n := &naturals{}
		rt.Set("nat", func() iter.Seq[int] { return n.seq() })
		if _, err := rt.Eval(`var kept = nat(); kept.next()`); err != nil {
			t.Fatal(err)
		}
		if n.stopped {
			t.Fatal("stopped before Close")
		}
		rt.Close()
		if !n.stopped {
			t.Error("Close did not stop a sequence script was still using")
		}
	})
	t.Run("collected", func(t *testing.T) {
		rt := quickjs.New()
		defer rt.Close()
		n := &naturals{}
		rt.Set("nat", func() iter.Seq[int] { return n.seq() })
		if _, err := rt.Eval(`(function () { var it = nat(); it.next() })()`); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for !n.stopped && time.Now().Before(deadline) {
			runtime.GC()
			time.Sleep(10 * time.Millisecond)
			if err := rt.RunJobs(); err != nil {
				t.Fatal(err)
			}
		}
		if !n.stopped {
			t.Error("a dropped iterator's sequence was not stopped")
		}
	})
}

// TestGoAsyncFunctions covers Go: work on a goroutine of its own whose
// result settles a promise script awaits, a Go error rejecting it as a
// thrown one would, a result that cannot be converted rejecting it, and a
// runtime that closes cancelling the work's context.
func TestGoAsyncFunctions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rt := quickjs.New()
	defer rt.Close()
	type user struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	rt.Set("fetchUser", func(id int) quickjs.Value {
		return rt.Go(func(ctx context.Context) (any, error) {
			time.Sleep(5 * time.Millisecond)
			if id < 0 {
				return nil, errors.New("no such user")
			}
			return user{ID: id, Name: "Ada"}, nil
		})
	})
	rt.Set("badResult", func() quickjs.Value {
		return rt.Go(func(context.Context) (any, error) { return make(chan int), nil })
	})
	p, err := rt.Eval(`(async () => {
		const u = await fetchUser(7);
		let missing;
		try { await fetchUser(-1) } catch (e) { missing = e.constructor.name + ": " + e.message }
		let bad;
		try { await badResult() } catch (e) { bad = e.constructor.name }
		return u.name + "#" + u.id + " | " + missing + " | " + bad;
	})()`)
	if err != nil {
		t.Fatal(err)
	}
	v, err := p.Await(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := v.String(), "Ada#7 | Error: no such user | Error"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// Close cancels what Go started.
	rt2 := quickjs.New()
	cancelled := make(chan struct{})
	started := make(chan struct{})
	rt2.Set("wait", func() quickjs.Value {
		return rt2.Go(func(ctx context.Context) (any, error) {
			close(started)
			<-ctx.Done()
			close(cancelled)
			return nil, ctx.Err()
		})
	})
	if _, err := rt2.Eval(`wait()`); err != nil {
		t.Fatal(err)
	}
	<-started
	rt2.Close()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Error("Close did not cancel the context Go's work was given")
	}
}

// TestAwait covers Value.Await: a promise settled by jobs, by a host's work
// and by Go's; a value and a thenable, awaited as script's await takes them;
// a rejection, which is an *Error and counts as handled; a promise nothing
// can settle; a context that ends; and a call made while script runs, which
// is refused.
func TestAwait(t *testing.T) {
	ctx := context.Background()
	rt := quickjs.New()
	defer rt.Close()
	unhandled := 0
	rt.OnUnhandledRejection(func(quickjs.Value) { unhandled++ })

	await := func(src string) (quickjs.Value, error) {
		t.Helper()
		v, err := rt.Eval(src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		return v.Await(ctx)
	}
	for _, tc := range []struct{ src, want string }{
		{`Promise.resolve(1).then(x => x + 1)`, "2"},
		{`42`, "42"},
		{`({ then(resolve) { resolve(7) } })`, "7"},
		{`(async () => { await null; return "done" })()`, "done"},
	} {
		v, err := await(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if v.String() != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, v.String(), tc.want)
		}
	}

	_, err := await(`Promise.reject(new TypeError("bad"))`)
	var jsErr *quickjs.Error
	if !errors.As(err, &jsErr) || !strings.Contains(err.Error(), "bad") {
		t.Errorf("a rejection: got %v, want the *Error it was rejected with", err)
	} else if name, _ := jsErr.Value().Get("name"); name.String() != "TypeError" {
		t.Errorf("a rejection's value is a %s, want the TypeError", name)
	}

	// A promise rejected while Await waits for it was taken by Await, and
	// is not reported as unhandled. (One rejected in a turn that ended with
	// no one taking it, as the one above was in Eval's, has been.)
	unhandled = 0
	rw := rt.StartAsyncWork()
	rp := rt.NewPromise()
	go rw.Complete(func(err error) {
		if err == nil {
			rp.Reject(rt.NewError("RangeError", "later"))
		}
	})
	if _, err := rp.Value().Await(ctx); err == nil || !strings.Contains(err.Error(), "later") {
		t.Errorf("a promise rejected while awaited: got %v", err)
	}
	if err := rt.RunJobs(); err != nil {
		t.Fatal(err)
	}
	if unhandled != 0 {
		t.Errorf("a rejection Await took was reported as unhandled")
	}

	if _, err := await(`new Promise(() => {})`); !errors.Is(err, quickjs.ErrNeverSettles) {
		t.Errorf("a promise nothing settles: got %v, want ErrNeverSettles", err)
	}

	// A promise a host's work will settle, waited for under a deadline it
	// does not meet.
	w := rt.StartAsyncWork()
	p := rt.NewPromise()
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := p.Value().Await(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a deadline: got %v, want context.DeadlineExceeded", err)
	}
	// And then settled from another goroutine.
	go w.Complete(func(err error) {
		if err == nil {
			p.Resolve("late")
		}
	})
	if v, err := p.Value().Await(ctx); err != nil || v.String() != "late" {
		t.Errorf("a promise a host's work settles: got %v, %v", v, err)
	}

	rt.Set("awaitInside", func(p quickjs.Value) error {
		_, err := p.Await(ctx)
		return err
	})
	v, err := rt.Eval(`try { awaitInside(Promise.resolve(1)); "no" } catch (e) { e.message }`)
	if err != nil || !strings.Contains(v.String(), "while a script is running") {
		t.Errorf("Await inside a call: got %v, %v", v, err)
	}
}
