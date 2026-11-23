package vm

import (
	"runtime"
	"strconv"
	"testing"
)

// Saved arguments of returned calls must not keep their values alive between
// turns. This also covers the options object retaining a removed AbortSignal.
func TestReturnedArgumentsCollected(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, tc := range []struct{ name, body string }{
			{"return", `return arguments.length`},
			{"throw", `if (arguments.length) throw 42`},
			{"forward", `return count.apply(null, arguments)`},
		} {
			t.Run(tc.name+"/native="+strconv.FormatBool(native), func(t *testing.T) {
				r := newTestRuntime(t, Config{}.withNative(native))
				defer r.Close()
				fn := compileForTest(t, `
					var refs = [];
					function count() { return arguments.length; }
					function inspect(value) { `+tc.body+` }
					(function () {
						for (let i = 0; i < 100; i++) {
							const value = {};
							refs.push(new WeakRef(value));
							try { inspect({signal: value}); } catch (e) { if (e !== 42) throw e; }
						}
					})();
					0;
				`)
				if _, err := r.Run(fn); err != nil {
					t.Fatal(err)
				}
				if len(r.argStack) != 0 || cap(r.argStack) == 0 {
					t.Fatalf("saved argument buffer: len=%d cap=%d", len(r.argStack), cap(r.argStack))
				}
				for range 4 {
					runtime.GC()
				}
				v, err := r.Run(compileForTest(t, `refs.every(ref => ref.deref() === undefined)`))
				if err != nil {
					t.Fatal(err)
				}
				if !v.BoolValue() {
					t.Fatal("returned call's argument is still alive")
				}
			})
		}
	}
}

// A sweep during a call clears only returned calls' saved arguments: the
// running call and arguments objects that escaped it still own their values.
func TestArgumentSweepPreservesLiveArguments(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run("native="+strconv.FormatBool(native), func(t *testing.T) {
			r := newTestRuntime(t, Config{}.withNative(native))
			defer r.Close()
			r.global.setOwnRaw(r.atoms.intern("sweep"), Obj(r.newNativeFunc("sweep", 0,
				func(rt *Runtime, this Value, args []Value) (Value, error) {
					rt.clearReturnedFrames()
					return Undefined, nil
				})), propDefault)
			v, err := r.Run(compileForTest(t, `
				var escaped;
				function inner() { return arguments.length; }
				function outer(value) {
					inner({}, {}, {}, {}, {});
					sweep();
					if (arguments.length !== 1 || arguments[0].answer !== 42) throw Error("live arguments lost");
					escaped = arguments;
				}
				outer({answer: 42});
				0;
			`))
			if err != nil {
				t.Fatal(err)
			}
			v, err = r.Run(compileForTest(t, `escaped.length === 1 && escaped[0].answer === 42`))
			if err != nil || !v.BoolValue() {
				t.Fatalf("escaped arguments lost: %v, %v", v, err)
			}
		})
	}
}
