package quickjs_test

import (
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestPromiseSubclassThenSettlesInItsTurn pins that the promise a
// subclass's then makes is the reaction's result capability, settled by
// the reaction's one job, so that it settles in the same turn as an
// intrinsic promise made beside it (KI-85). It had been settled through a
// second then on an intrinsic promise, a turn late at each step: the
// first case was "qm,intrinsic,subclass", and a subclass's finally two
// turns late. The answers are Node 26's, in standards mode and under
// WithNodeQuirks alike.
func TestPromiseSubclassThenSettlesInItsTurn(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"then", `class P extends Promise {} P.resolve(1).then(v => v).then(() => log.push("subclass")); Promise.resolve(1).then(v => v).then(() => log.push("intrinsic")); queueMicrotask(() => log.push("qm"))`,
			"qm,subclass,intrinsic"},
		{"finally", `class P extends Promise {} P.resolve(1).finally(() => {}).then(() => log.push("P")); Promise.resolve(1).finally(() => {}).then(() => log.push("I")); var t = 0; (function tick() { log.push(t++); if (t < 7) queueMicrotask(tick) })()`,
			"0,1,2,3,P,I,4,5,6"},
		{"catch", `class P extends Promise {} P.reject(1).catch(v => v).then(() => log.push("P")); Promise.reject(1).catch(v => v).then(() => log.push("I"))`,
			"P,I"},
		{"chain", `class P extends Promise {} var p = P.resolve(0), q = Promise.resolve(0); for (var i = 0; i < 4; i++) { p = p.then(v => v + 1); q = q.then(v => v + 1) } p.then(v => log.push("P" + v)); q.then(v => log.push("I" + v))`,
			"P4,I4"},
		{"passThrough", `class P extends Promise {} P.reject(new RangeError("r")).then(v => v).then(null, e => log.push("P " + e.name)); Promise.reject(new RangeError("r")).then(v => v).then(null, e => log.push("I " + e.name))`,
			"P RangeError,I RangeError"},
		{"pending", `class P extends Promise {} var res; var p = new P(r => res = r); p.then(v => log.push("P" + v)); var q = Promise.resolve().then(() => log.push("I")); res(5); log.push(p.then(() => {}) instanceof P)`,
			"true,I,P5"},
		{"await", `class P extends Promise {} (async () => { await P.resolve(1); log.push("await P") })(); (async () => { await Promise.resolve(1); log.push("await I") })(); Promise.resolve().then(() => log.push("a")).then(() => log.push("b")).then(() => log.push("c"))`,
			"await I,a,b,await P,c"},
	}
	for _, quirks := range []bool{false, true} {
		for _, tt := range cases {
			var opts []quickjs.Option
			if quirks {
				opts = append(opts, quickjs.WithNodeQuirks())
			}
			rt := quickjs.New(opts...)
			if _, err := rt.Eval("var log = []; " + tt.src); err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
			v, err := rt.Eval(`log.join()`)
			if err != nil || v.String() != tt.want {
				t.Errorf("%s (quirks %v): %v, %v; want %q", tt.name, quirks, v, err, tt.want)
			}
			rt.Close()
		}
	}
}

// TestPromiseCapabilityResolveThrows pins what a reaction's job does when
// a subclass's resolve or reject, its result capability's, throws. In
// standards mode the exception is the job's, which reaches the host
// (OnUncaughtException), and nothing else is called. Under WithNodeQuirks,
// as in Node, a resolve that throws is followed by the capability's reject
// with the exception, and where the handler ran and returned, a reject
// that throws then by the reject again with that one; the last exception
// reaches the host.
func TestPromiseCapabilityResolveThrows(t *testing.T) {
	// Each case's capability resolve throws "res" when res is set, and its
	// reject logs what it is given and throws "rej" when rej is set.
	cases := []struct {
		name, res, rej, then string
		want, quirks         string
	}{
		{"resolveThrows", "true", "false", "v => v",
			"|uncaught Error: res", "reject res|"},
		{"bothThrow", "true", "true", "v => v",
			"|uncaught Error: res", "reject res,reject rej|uncaught Error: rej"},
		{"bothThrowNoHandler", "true", "true", "",
			"|uncaught Error: res", "reject res|uncaught Error: rej"},
		{"handlerAndRejectThrow", "false", "true", "() => { throw new Error('h') }",
			"reject h|uncaught Error: rej", "reject h|uncaught Error: rej"},
	}
	for _, quirks := range []bool{false, true} {
		for _, tt := range cases {
			var opts []quickjs.Option
			if quirks {
				opts = append(opts, quickjs.WithNodeQuirks())
			}
			rt := quickjs.New(opts...)
			rt.OnUnhandledRejection(func(quickjs.Value) {})
			var uncaught []string
			rt.OnUncaughtException(func(err error) { uncaught = append(uncaught, "uncaught "+err.Error()) })
			src := `var n = 0, log = []; class P extends Promise { constructor(ex) { super((res, rej) => ex(v => { if (n++ && ` + tt.res + `) throw new Error('res'); res(v) }, e => { log.push('reject ' + (e && e.message)); if (` + tt.rej + `) throw new Error('rej'); rej(e) })) } } new P(r => r(1)).then(` + tt.then + `)`
			if _, err := rt.Eval(src); err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
			v, err := rt.Eval(`log.join()`)
			if err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
			got := v.String() + "|" + strings.Join(uncaught, ",")
			want := tt.want
			if quirks {
				want = tt.quirks
			}
			if got != want {
				t.Errorf("%s (quirks %v): %q, want %q", tt.name, quirks, got, want)
			}
			rt.Close()
		}
	}
}
