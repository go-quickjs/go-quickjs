package stdlib_test

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/stdlib"
)

// TestAsyncLocalStorageLikeNode pins AsyncLocalStorage as node 26 has it,
// whose output for this very script is the want below: a store survives a
// then, an await, a microtask, a thenable, Promise.all, a timeout and an
// interval; a reaction runs in the context it was registered in, not the one
// the promise was settled in; run nests and exit leaves; snapshot,
// AsyncResource and bind keep the context they were made in; and enterWith
// holds for the rest of the turn and its continuations.
func TestAsyncLocalStorageLikeNode(t *testing.T) {
	out, errOut := run(t, stdlib.Config{}, `
import("node:async_hooks").then(({ AsyncLocalStorage, AsyncResource }) => {
const als = new AsyncLocalStorage();
const out = [];
const log = (tag) => out.push(tag + ":" + als.getStore());

log("outside");
als.run("A", () => {
  log("sync");
  Promise.resolve().then(() => log("then"));
  (async () => {
    await null;
    log("await");
    await new Promise((r) => setTimeout(r, 5));
    log("after-timeout");
  })();
  setTimeout(() => log("timeout"), 1);
  queueMicrotask(() => log("microtask"));
  const thenable = { then(res) { log("thenable"); res(1); } };
  Promise.resolve(thenable).then(() => log("thenable-then"));
  als.run("B", () => { Promise.resolve().then(() => log("nested-then")); });
  als.exit(() => log("exit"));
  Promise.all([Promise.resolve(1), new Promise((r) => setTimeout(r, 2))]).then(() => log("all"));
});
log("after-run");

let resolveIt;
const p = als.run("C", () => new Promise((r) => { resolveIt = r; }));
p.then(() => log("registered-outside"));
als.run("D", () => resolveIt());

const snap = als.run("E", () => AsyncLocalStorage.snapshot());
snap(() => log("snapshot"));
const res = als.run("F", () => new AsyncResource("X"));
res.runInAsyncScope(() => log("resource"));
const bound = als.run("G", () => AsyncLocalStorage.bind(() => log("bound")));
bound();

const interval = als.run("H", () => setInterval(() => { log("interval"); clearInterval(interval); }, 3));

als.enterWith("I");
log("enterWith");
Promise.resolve().then(() => log("enterWith-then"));

setTimeout(() => console.log(out.join(" ")), 50);
});
`)
	want := "outside:undefined sync:A exit:undefined after-run:undefined snapshot:E resource:F bound:G " +
		"enterWith:I then:A await:A microtask:A thenable:A nested-then:B registered-outside:undefined " +
		"enterWith-then:I thenable-then:A timeout:A all:A interval:H after-timeout:A"
	if out != want {
		t.Errorf("got\n%s\nwant\n%s\nstderr: %s", out, want, errOut)
	}
}
