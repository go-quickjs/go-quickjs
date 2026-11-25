package vm

import "testing"

// TestPromiseReactionAllocations pins what queueing a settled promise's
// reaction allocates: nothing for an await's, which is a job of its own
// kind, and only the job's function for a then's, an intrinsic promise's
// or a subclass's capability alike. A job's function that took the
// reaction's address had the reaction moved to the heap on every call,
// an await's included: an allocation more each await (async_chain's three
// a step).
func TestPromiseReactionAllocations(t *testing.T) {
	r := newTestRuntime(t, Config{})
	defer r.Close()
	settled := &promiseData{state: promiseFulfilled, value: Int(1)}
	handler, err := r.Run(compileForTest(t, `(v => v)`))
	if err != nil {
		t.Fatal(err)
	}
	result := r.newPromise()
	cap := &promiseCapability{promise: result, resolve: handler, reject: handler}
	for _, tt := range []struct {
		name string
		rc   reaction
		want float64
	}{
		{"await", reaction{ctx: Undefined, gen: &generator{}, resume: awaitAsync}, 0},
		{"then", reaction{onFulfilled: handler, onRejected: handler, result: result, ctx: Undefined}, 1},
		{"capability", reaction{onFulfilled: handler, onRejected: handler, cap: cap, ctx: Undefined}, 1},
	} {
		r.microtasks = make([]job, 0, 4)
		if n := testing.AllocsPerRun(100, func() {
			r.microtasks = r.microtasks[:0]
			r.enqueueReaction(settled, tt.rc)
		}); n != tt.want {
			t.Errorf("%s: %v allocations, want %v", tt.name, n, tt.want)
		}
	}
	r.microtasks = nil
}
