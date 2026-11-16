package vm

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/compiler"
)

// The native handlers and realms: a frame's context (nativeCtx) is made
// once and kept with its state on the runtime's list of them (regFree),
// which every realm's frames take from, so that what the context holds is
// the runtime's or is read through the realm running now -- never one
// realm's, which the next frame's realm may not be and which goes when a
// host drops it.

// nativeInRealmSrc runs the in operator by a string key, the in cache's
// answer, many times in a loop of its own.
const nativeInRealmSrc = `
	function f(o) { var n = 0; for (var i = 0; i < 200; i++) if ("x" in o) n++; return n; }
	f({x: 1}) + f({y: 1});`

// nativeRealmFn compiles src in the register form, with the register tier
// and the native handlers on for the test.
func nativeRealmFn(t *testing.T, src string) *bytecode.Function {
	t.Helper()
	compiler.SetRegisterForm(true)
	fn := compileForTest(t, src)
	compiler.SetRegisterForm(bytecode.RegDefault())
	SetRegTier(true)
	was := regNativeOn
	regNativeOn = true
	t.Cleanup(func() { SetRegTier(bytecode.RegDefault()); regNativeOn = was })
	return fn
}

// nativeRunInRealm runs fn in re and checks its answer.
func nativeRunInRealm(t *testing.T, r *Runtime, re *Realm, fn *bytecode.Function) {
	t.Helper()
	v, err := r.RunIn(re, fn)
	if err != nil {
		t.Fatal(err)
	}
	if v.Number() != 200 {
		t.Fatalf("got %v, want 200", v)
	}
}

// The in operator's native handler answers from the cache of the realm
// running now, which Go fills: it had read the cache of the realm the
// frame's context was made in, the address of that realm's field kept in
// the context, so that in any other realm every in exited to Go, which
// filled a cache the code never read.
func TestRegNativeInRealmCache(t *testing.T) {
	fn := nativeRealmFn(t, nativeInRealmSrc)
	nativeCounting(t)
	r := New(Config{})
	defer r.Close()
	nativeRunInRealm(t, r, r.NewRealm(), fn)
	b := r.NewRealm()
	before := nativeExitOps()[bytecode.RIn]
	nativeRunInRealm(t, r, b, fn)
	// The first of each object's layouts exits, to fill the cache.
	if n := nativeExitOps()[bytecode.RIn] - before; n > 2 {
		t.Errorf("%d of 400 in operators exited to Go in the second realm, want 2 at most", n)
	}
}

// nativeClobberChild says the process is TestRegNativeInDroppedRealm's
// child, run with the collector clobbering what it frees.
const nativeClobberChild = "QJS_TEST_NATIVE_CLOBBER_CHILD"

// A realm a host has dropped is collected while the runtime's frames'
// contexts live on, and the next realm's in operator reads nothing of it:
// the context had kept the address of the first realm's in cache field,
// which no collector sees, and read it once the realm was gone, a fault
// under load in TestDroppedRealmsAreCollected. The test runs itself again
// with GODEBUG=clobberfree=1, which fills what the collector frees with a
// bad address, so that a read of the dropped realm faults every time.
func TestRegNativeInDroppedRealm(t *testing.T) {
	if os.Getenv(nativeClobberChild) == "" {
		godebug := "clobberfree=1"
		if g := os.Getenv("GODEBUG"); g != "" {
			godebug = g + "," + godebug
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestRegNativeInDroppedRealm$", "-test.count=1", "-test.v")
		cmd.Env = append(os.Environ(), nativeClobberChild+"=1", "GODEBUG="+godebug)
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "--- PASS: TestRegNativeInDroppedRealm") {
			t.Fatalf("child: %v\n%s", err, out)
		}
		return
	}
	fn := nativeRealmFn(t, nativeInRealmSrc)
	r := New(Config{})
	defer r.Close()
	ran := func() weak.Pointer[Realm] {
		a := r.NewRealm()
		nativeRunInRealm(t, r, a, fn)
		return weak.Make(a)
	}
	for range 4 {
		if n := realmsAlive(r, []weak.Pointer[Realm]{ran()}); n != 0 {
			t.Fatal("the dropped realm was not collected")
		}
		nativeRunInRealm(t, r, r.NewRealm(), fn)
	}
}
