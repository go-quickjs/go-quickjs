package stdlib_test

import (
	"context"
	"strings"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/stdlib"
)

// TestStructuredCloneMatchesNode pins structuredClone to what node 26 does
// with each kind of value, word for word where it throws.
func TestStructuredCloneMatchesNode(t *testing.T) {
	out, _ := run(t, stdlib.Config{}, `
		const log = (...a) => console.log(...a);
		const t = (label, f) => { try { log(label, f()) } catch (e) { log(label, "THROWS", e.constructor.name, e.name, e.code, JSON.stringify(e.message)) } };
		const c = structuredClone;
		t("prims", () => [c(1), c("s"), c(10n), c(null), c(undefined), c(true), Object.is(c(-0), -0), c(NaN)].map(String).join());
		t("symbol", () => c(Symbol()));
		t("function", () => c(() => 1));
		t("wrappers", () => { const r = c([Object(1), Object("s"), Object(true), Object(2n)]); return r.map(x => typeof x + ":" + x.valueOf()).join() });
		t("date", () => { const d = c(new Date(5)); return d instanceof Date && d.getTime() });
		t("regexp", () => { const r = /a/gi; r.lastIndex = 3; r.x = 1; const k = c(r); return [k.source, k.flags, k.lastIndex, k.x].join() });
		t("sparse", () => { const a = [1, , 3]; a.x = "y"; a.length = 5; const k = c(a); return [k.length, 1 in k, k[2], k.x].join() });
		t("object", () => { const o = { a: 1, [Symbol()]: 2 }; Object.defineProperty(o, "h", { value: 3, enumerable: false }); Object.defineProperty(o, "g", { get() { return 4 }, enumerable: true }); return JSON.stringify(c(o)) });
		t("class instance", () => { class P { constructor() { this.x = 1 } m() {} } const k = c(new P()); return [k.constructor === Object, k.x].join() });
		t("cycle", () => { const o = {}; o.self = o; const a = [o, o]; const k = c(a); return [k[0] === k[1], k[0].self === k[0]].join() });
		t("map/set", () => { const k = { v: 1 }; const m = new Map([[k, k]]); const r = c([m, new Set([k])]); const [m2, s2] = r; const [kk, vv] = [...m2][0]; return [kk === vv, [...s2][0] === kk, m2 instanceof Map].join() });
		t("error", () => { const e = new RangeError("m", { cause: { z: 1 } }); e.extra = 1; const k = c(e); return [k.constructor.name, k.name, k.message, JSON.stringify(k.cause), k.extra, typeof k.stack, k.stack === e.stack, Object.getOwnPropertyNames(k).join("|")].join(",") });
		t("error custom name", () => { const e = new Error("m"); e.name = "Custom"; const k = c(e); return [k.name, k.constructor.name].join() });
		t("error subclass", () => { class My extends TypeError {} const k = c(new My("x")); return [k.constructor.name, k.message].join() });
		t("aggregate", () => { const k = c(new AggregateError([1], "a")); return [k.constructor.name, k.message, JSON.stringify(k.errors)].join() });
		t("error no message", () => { const k = c(new Error()); return [Object.getOwnPropertyNames(k).join("|"), "message" in k].join() });
		t("arraybuffer", () => { const b = new ArrayBuffer(4, { maxByteLength: 8 }); new Uint8Array(b)[0] = 7; const k = c(b); return [k !== b, k.byteLength, k.resizable, k.maxByteLength, new Uint8Array(k)[0]].join() });
		t("views", () => { const b = new ArrayBuffer(8); const u = new Uint16Array(b, 2, 2); const d = new DataView(b, 1, 3); const [u2, d2] = c([u, d]); return [u2.constructor.name, u2.byteOffset, u2.length, u2.buffer === d2.buffer, d2.byteOffset, d2.byteLength].join() });
		t("tracking view", () => { const b = new ArrayBuffer(4, { maxByteLength: 8 }); const u = c(new Uint8Array(b)); u.buffer.resize(8); return u.length });
		t("transfer", () => { const b = new ArrayBuffer(4); new Uint8Array(b)[0] = 9; const k = c({ b }, { transfer: [b] }); return [b.byteLength, b.detached, k.b.byteLength, new Uint8Array(k.b)[0]].join() });
		t("transfer twice", () => { const b = new ArrayBuffer(4); return c(b, { transfer: [b, b] }) });
		t("transfer detached", () => { const b = new ArrayBuffer(4); b.transfer(); return c(1, { transfer: [b] }) });
		t("transfer non-transferable", () => c(1, { transfer: [{}] }));
		t("sab", () => { const s = new SharedArrayBuffer(4); const k = c(s); new Int32Array(k)[0] = 5; return [k !== s, new Int32Array(s)[0]].join() });
		t("transfer sab", () => { const s = new SharedArrayBuffer(4); return c(s, { transfer: [s] }) });
		t("promise", () => c(Promise.resolve()));
		t("weakmap", () => c(new WeakMap()));
		t("proxy", () => c(new Proxy({}, {})));
		t("proxy array", () => c(new Proxy([], {})));
		t("getter throws", () => c({ get x() { throw new RangeError("g") } }));
		t("options", () => c(1, { transfer: undefined }));
		t("options bad", () => c(1, { transfer: 5 }));
		t("detached view", () => { const b = new ArrayBuffer(4); const u = new Uint8Array(b); b.transfer(); return c(u) });
		t("DOMException", () => { const e = c(new DOMException("m", "DataCloneError")); return [e.constructor.name, e.name, e.message, e.code].join() });
		t("DOMException shape", () => { const e = new DOMException("m", "DataCloneError"); return [e instanceof Error, Object.getOwnPropertyNames(e).join("|"), DOMException.DATA_CLONE_ERR, e.code, typeof e.stack, Object.prototype.toString.call(e)].join() });
		t("Object.prototype tag spoof", () => { const o = { [Symbol.toStringTag]: "Date" }; return JSON.stringify(c(o)) });
		t("boxed with props", () => { const n = Object(1); n.x = 1; return JSON.stringify(Object.keys(c(n))) });
	`)
	want := `
prims 1,s,10,null,undefined,true,true,NaN
symbol THROWS DOMException DataCloneError 25 "Symbol() could not be cloned."
function THROWS DOMException DataCloneError 25 "() => 1 could not be cloned."
wrappers object:1,object:s,object:true,object:2
date 5
regexp a,gi,0,
sparse 5,false,3,y
object {"a":1,"g":4}
class instance true,1
cycle true,true
map/set true,true,true
error RangeError,RangeError,m,{"z":1},,string,true,stack|message|cause
error custom name Error,Error
error subclass TypeError,x
aggregate Error,a,
error no message stack,true
arraybuffer true,4,true,8,7
views Uint16Array,2,2,true,1,3
tracking view 8
transfer 0,true,4,9
transfer twice THROWS DOMException DataCloneError 25 "Transfer list contains duplicate ArrayBuffer"
transfer detached THROWS DOMException DataCloneError 25 "Cannot transfer object of unsupported type."
transfer non-transferable THROWS DOMException DataCloneError 25 "Found invalid value in transferList."
sab true,5
transfer sab THROWS DOMException DataCloneError 25 "Found invalid value in transferList."
promise THROWS DOMException DataCloneError 25 "#<Promise> could not be cloned."
weakmap THROWS DOMException DataCloneError 25 "#<WeakMap> could not be cloned."
proxy THROWS DOMException DataCloneError 25 "#<Object> could not be cloned."
proxy array THROWS DOMException DataCloneError 25 "[object Array] could not be cloned."
getter throws THROWS RangeError RangeError undefined "g"
options 1
options bad THROWS TypeError TypeError ERR_INVALID_ARG_TYPE "Failed to execute 'structuredClone': transfer in Options cannot be converted to sequence."
detached view THROWS DOMException DataCloneError 25 "An ArrayBuffer is detached and could not be cloned."
DOMException DOMException,DataCloneError,m,25
DOMException shape true,stack,25,25,string,[object DOMException]
Object.prototype tag spoof {}
boxed with props []
`
	if want = strings.TrimSpace(want); out != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
}

// TestStructuredCloneArguments pins how structuredClone reads its arguments,
// as node does -- but for the message of a missing value, which node garbles.
func TestStructuredCloneArguments(t *testing.T) {
	out, _ := run(t, stdlib.Config{}, `
		for (const f of [() => structuredClone(), () => structuredClone(1, 5), () => structuredClone(1, {transfer: 5})]) {
			try { f() } catch (e) { console.log(e.name, e.code, e.message) }
		}
		console.log(structuredClone(1, null), structuredClone(2, {transfer: undefined}))
	`)
	want := strings.Join([]string{
		`TypeError ERR_MISSING_ARGS The "value" argument must be specified`,
		`TypeError ERR_INVALID_ARG_TYPE Failed to execute 'structuredClone': Options cannot be converted to a dictionary`,
		`TypeError ERR_INVALID_ARG_TYPE Failed to execute 'structuredClone': transfer in Options cannot be converted to sequence.`,
		`1 2`,
	}, "\n")
	if out != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
}

// TestMessageChannel pins a channel's messages: dispatched a task at a time
// after the microtasks, what was sent before the port started kept for it, a
// transferred buffer moved, and a close heard at both ends -- after which
// nothing holds the loop.
func TestMessageChannel(t *testing.T) {
	out, _ := run(t, stdlib.Config{}, `
		const {port1, port2} = new MessageChannel();
		const order = [];
		port1.postMessage("before");
		const b = new ArrayBuffer(4);
		new Uint8Array(b)[0] = 7;
		port1.postMessage({b, when: new Date(3)}, [b]);
		order.push("detached " + b.detached);
		port2.onmessage = (e) => {
			const d = e.data;
			order.push([e.constructor.name, e.target === port2, e.ports.length,
				typeof d === "string" ? d : new Uint8Array(d.b)[0] + "," + d.when.getTime()].join(" "));
			if (typeof d !== "string") port2.close();
		};
		Promise.resolve().then(() => order.push("micro"));
		port2.addEventListener("close", () => order.push("close2"));
		port1.addEventListener("close", () => { order.push("close1"); console.log(order.join("|")) });
		console.log(Object.keys(new MessageChannel()).join(), MessagePort.prototype.postMessage.length,
			Object.prototype.toString.call(port1), port1 instanceof EventTarget)
		try { new MessagePort() } catch (e) { console.log(e.name, e.code, e.message) }
	`)
	want := strings.Join([]string{
		"port1,port2 1 [object MessagePort] true",
		"TypeError ERR_CONSTRUCT_CALL_INVALID Constructor cannot be called",
		"detached true|micro|MessageEvent true 0 before|MessageEvent true 0 7,3|close2|close1",
	}, "\n")
	if out != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
}

// TestMessagePortTransfer pins posting a port through another: what was sent
// to it on the way arrives where it went, the object left behind is inert,
// and a transfer list is checked as node checks it.
func TestMessagePortTransfer(t *testing.T) {
	out, _ := run(t, stdlib.Config{}, `
		const a = new MessageChannel(), b = new MessageChannel();
		const errs = [];
		const attempt = (f) => { try { f() } catch (e) { errs.push(e.name + ": " + e.message) } };
		attempt(() => a.port1.postMessage(1, [a.port1]));
		attempt(() => b.port1.postMessage(a.port2));
		attempt(() => b.port1.postMessage(1, [a.port2, a.port2]));
		a.port1.postMessage("sent on the way");
		b.port1.postMessage({p: a.port2}, {transfer: [a.port2]});
		attempt(() => b.port1.postMessage(1, [a.port2]));
		attempt(() => structuredClone(a.port2, {transfer: [a.port2]}));
		a.port2.postMessage("from the inert port");
		b.port2.onmessage = (e) => {
			const p = e.ports[0];
			console.log(errs.join("\n"));
			console.log(p === e.data.p, p !== a.port2, e.ports.length, Object.isFrozen(e.ports));
			p.onmessage = (e) => { console.log("moved", e.data); p.close(); b.port1.close(); };
		};
	`)
	want := strings.Join([]string{
		"DataCloneError: Transfer list contains source port",
		"DataCloneError: Object that needs transfer was found in message but not listed in transferList",
		"DataCloneError: Transfer list contains duplicate MessagePort",
		"DataCloneError: MessagePort in transfer list is already detached",
		"DataCloneError: MessagePort in transfer list is already detached",
		"true true 1 true",
		"moved sent on the way",
	}, "\n")
	if out != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
}

// messagingLoop is a runtime with the standard library and its loop.
func messagingLoop(t *testing.T) (*quickjs.Runtime, *stdlib.Loop) {
	t.Helper()
	rt := quickjs.New()
	t.Cleanup(func() { rt.Close() })
	loop := stdlib.NewLoop(rt)
	if err := stdlib.Install(rt, stdlib.Config{Loop: loop}); err != nil {
		t.Fatal(err)
	}
	return rt, loop
}

// TestMessagePortHoldsTheLoop pins that a listening port keeps the loop
// running, as node's does, until it is unref'd.
func TestMessagePortHoldsTheLoop(t *testing.T) {
	rt, loop := messagingLoop(t)
	if _, err := rt.Eval(`var c = new MessageChannel(); c.port2.onmessage = () => {};
		c.port1.postMessage(1)`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := loop.Run(ctx); err != context.DeadlineExceeded {
		t.Fatalf("Run = %v, want the deadline: the port holds the loop", err)
	}
	v, err := rt.Eval(`const held = c.port2.hasRef(); c.port2.unref(); [held, c.port2.hasRef(), c.port1.hasRef()].join()`)
	if err != nil || v.String() != "true,false,false" {
		t.Fatalf("hasRef = %v, %v", v, err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := loop.Run(ctx); err != nil {
		t.Fatalf("Run = %v once the port is unref'd", err)
	}
}

// TestMessagePortListenerError pins that an exception a listener throws is
// uncaught, as one a timer's callback throws is.
func TestMessagePortListenerError(t *testing.T) {
	rt, loop := messagingLoop(t)
	if _, err := rt.Eval(`const c = new MessageChannel();
		c.port2.onmessage = (e) => { throw new RangeError("from a listener " + e.data) };
		c.port1.postMessage(1)`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := loop.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "RangeError: from a listener 1") {
		t.Fatalf("Run = %v, want the listener's exception", err)
	}
}

// TestMessageChannelWithoutLoop pins that a runtime given the web APIs and no
// loop dispatches a port's messages when it runs its jobs.
func TestMessageChannelWithoutLoop(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	if err := stdlib.WebAPIs(rt, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Eval(`var got = []; const c = new MessageChannel();
		c.port2.onmessage = (e) => got.push(e.data);
		c.port1.postMessage("a"); c.port1.postMessage("b")`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if err := rt.RunJobs(); err != nil {
			t.Fatal(err)
		}
	}
	if v, err := rt.Eval(`got.join()`); err != nil || v.String() != "a,b" {
		t.Fatalf("got = %v, %v", v, err)
	}
}

// TestDOMException pins the DOMException interface: an Error, named and
// coded as it is told, with the legacy constants.
func TestDOMException(t *testing.T) {
	out, _ := run(t, stdlib.Config{}, `
		const e = new DOMException("m", "AbortError");
		console.log(e instanceof Error, e.name, e.message, e.code, Object.getOwnPropertyNames(e).join(),
			Object.prototype.toString.call(e), String(e))
		const d = new DOMException();
		console.log(d.name, JSON.stringify(d.message), d.code, DOMException.ABORT_ERR, d.DATA_CLONE_ERR)
		const o = new DOMException("x", {name: "DataCloneError", cause: 1});
		console.log(o.name, o.code, o.cause, new DOMException("y", "Custom").code)
		try { DOMException.prototype.name } catch (err) { console.log(err.name, err.message) }
	`)
	want := strings.Join([]string{
		"true AbortError m 20 stack [object DOMException] AbortError: m",
		`Error "" 0 20 25`,
		"DataCloneError 25 1 0",
		`TypeError Value of "this" must be of DOMException`,
	}, "\n")
	if out != want {
		t.Errorf("got\n%s\nwant\n%s", out, want)
	}
}

// TestDroppedMessagesClosePorts pins that the ports inside a message that is
// never delivered are closed, as node closes them, so their other ends hear
// it: they stayed entangled with nothing holding them, and a started port
// held its loop forever (KI-23).
func TestDroppedMessagesClosePorts(t *testing.T) {
	const listen = `
		const c = new MessageChannel();
		c.port2.onmessage = () => {};
		c.port2.addEventListener("close", () => console.log("closed"));`
	for name, src := range map[string]string{
		"posted to a closed port": `const a = new MessageChannel(); a.port2.close();` + listen + `
			a.port1.postMessage(null, [c.port1]);`,
		"queued on a port that closes": `const a = new MessageChannel();` + listen + `
			a.port1.postMessage(null, [c.port1]);
			a.port2.close();`,
		"unread by a worker that ends": `const w = new require_worker_threads.Worker("./idle.mjs");` + listen + `
			w.postMessage(null, [c.port1]);`,
	} {
		t.Run(name, func(t *testing.T) {
			cfg := stdlib.Config{Workers: workerFiles(map[string]string{"./idle.mjs": ``})}
			if out, _ := run(t, cfg, src); out != "closed" {
				t.Errorf("got %q", out)
			}
		})
	}
}
