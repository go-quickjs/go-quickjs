package quickjs_test

import (
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// asyncContextRuntime is a runtime whose script reads and sets the async
// context through two Go functions, as a host's AsyncLocalStorage does.
func asyncContextRuntime(t *testing.T) (*quickjs.Runtime, *[]string) {
	t.Helper()
	rt := quickjs.New()
	t.Cleanup(func() { rt.Close() })
	var log []string
	rt.Set("ctx", func(r *quickjs.Runtime) quickjs.Value { return r.AsyncContext() })
	rt.Set("setCtx", func(r *quickjs.Runtime, v quickjs.Value) { r.SetAsyncContext(v) })
	rt.Set("note", func(s string) { log = append(log, s) })
	return rt, &log
}

// The engine carries the async context through every job: a reaction runs in
// the context it was registered in, a job in the one it was queued in, and a
// context a job sets ends with it. A call from Go leaves the context as it
// found it.
func TestAsyncContextThroughJobs(t *testing.T) {
	rt, log := asyncContextRuntime(t)
	_, err := rt.Eval(`
		let settle;
		const p = new Promise((r) => { settle = r; });
		setCtx("registered");
		p.then(() => note("then:" + ctx()));
		(async () => { await p; note("await:" + ctx()); })();
		setCtx("settled");
		settle();
		setCtx("job");
		Promise.resolve().then(() => { setCtx("leaked?"); note("job:" + ctx()); });
		setCtx("next");
		Promise.resolve().then(() => note("next:" + ctx()));
		setCtx(undefined);
	`)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(*log, " "), "then:registered await:registered job:leaked? next:next"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if v := rt.AsyncContext(); !v.IsUndefined() {
		t.Errorf("after Eval, the context is %v, want undefined", v)
	}

	// A context the host sets is the script's, and is put back after.
	*log = nil
	prev := rt.SetAsyncContext(mustEval(t, rt, `"request 7"`))
	if !prev.IsUndefined() {
		t.Errorf("prev = %v, want undefined", prev)
	}
	if _, err := rt.Eval(`setCtx("changed"); note("in:" + ctx())`); err != nil {
		t.Fatal(err)
	}
	if got := rt.AsyncContext().String(); got != "request 7" {
		t.Errorf("after Eval, the context is %q, want the host's", got)
	}
}

// What is posted to an AsyncWork runs in the context the work was started in,
// and a job the host queues in the one it was queued in.
func TestAsyncContextThroughHostWork(t *testing.T) {
	rt, log := asyncContextRuntime(t)
	rt.SetAsyncContext(mustEval(t, rt, `"started"`))
	w := rt.StartAsyncWork()
	rt.EnqueueJob(func() { *log = append(*log, "job:"+rt.AsyncContext().String()) })
	rt.SetAsyncContext(mustEval(t, rt, `undefined`))
	go w.Complete(func(err error) {
		if err == nil {
			*log = append(*log, "work:"+rt.AsyncContext().String())
		}
	})
	if err := runLoop(loopContext(t), rt); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(*log, " "), "job:started work:started"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if v := rt.AsyncContext(); !v.IsUndefined() {
		t.Errorf("after the loop, the context is %v, want undefined", v)
	}
}

func mustEval(t *testing.T, rt *quickjs.Runtime, src string) quickjs.Value {
	t.Helper()
	v, err := rt.Eval(src)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
