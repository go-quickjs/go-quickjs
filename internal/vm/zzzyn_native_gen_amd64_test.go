package vm

import (
	"bytes"
	"os"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"testing"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/vm/internal/nativegen"
)

// The native handlers are generated offline by
// internal/vm/internal/nativegen (go generate ./internal/vm), from the
// package's layout, which its helper reads in a build with the
// quickjs_nativelayout tag (zzzyn_native_layout.go).

// nativeBuildTags are the build tags this test binary was built with.
func nativeBuildTags() []string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "-tags" && s.Value != "" {
				return strings.Split(s.Value, ",")
			}
		}
	}
	return nil
}

// nativeBuilds are what the generator makes from the layout of a build
// with the tags of their key (nativegen.Build).
var nativeBuilds = struct {
	sync.Mutex
	m map[string]*nativegen.Output
}{m: map[string]*nativegen.Output{}}

func nativeBuild(t *testing.T, tags ...string) *nativegen.Output {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the package to read its layout")
	}
	nativeBuilds.Lock()
	defer nativeBuilds.Unlock()
	key := strings.Join(tags, ",")
	if out := nativeBuilds.m[key]; out != nil {
		return out
	}
	out, err := nativegen.Build("amd64", tags...)
	if err != nil {
		t.Fatal(err)
	}
	nativeBuilds.m[key] = out
	return out
}

// nativeTestTables are the dispatchers' tables this binary has, by the
// generator's names: plain's, and the feedback dispatcher's in a build
// with the JIT (zzzyn_native_feedback_amd64_test.go).
var nativeTestTables = map[string]uintptr{"plain": nativePlain.addr}

// TestRegNativeGenerated checks that every generated file -- the
// handlers' and their plain table, and each other variant's, which a
// build with other tags has -- is what the generator makes now
// (nativegen.Generate). A change to the emitter or to the layouts its
// offsets come from fails it until the files are written again (go
// generate ./internal/vm).
func TestRegNativeGenerated(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the package to read its layout")
	}
	files, err := nativegen.Generate()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		old, err := os.ReadFile(f.Name)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(bytes.ReplaceAll(old, []byte("\r\n"), []byte("\n")), f.Data) {
			t.Errorf("%s is not what nativegen makes: go generate ./internal/vm", f.Name)
		}
	}
}

// TestRegNativeLinked checks that the blocks and the tables this binary
// links are what the generator makes from this build's layout: the build
// its helper runs has this test binary's tags. One block serves every
// build: what the code reads of a layout that a tag changes, it reads
// relative to a base the context gives (rtBase, ccBase), so that the
// block made under any tag is the one file; the test fails under a tag
// whose layouts would make it another.
func TestRegNativeLinked(t *testing.T) {
	tags := nativeBuildTags()
	out := nativeBuild(t, tags...)
	t.Logf("layout of a build with tags %q", tags)
	base := nativeHandlersAddr()
	if len(out.Dispatchers) != len(nativeTestTables) {
		t.Fatalf("the generator makes %d dispatchers for this build, the binary has %d", len(out.Dispatchers), len(nativeTestTables))
	}
	for i, d := range out.Dispatchers {
		at, ok := nativeTestTables[d.Name]
		if !ok {
			t.Fatalf("the binary has no %s dispatcher", d.Name)
		}
		tab := unsafe.Slice((*uintptr)(nativePtr(&at)), len(d.Table))
		// A variant's own block is where its own entries say.
		own := uintptr(0)
		if i == 0 {
			own = base
		}
		for n, e := range d.Table {
			if e.Own && own == 0 {
				own = tab[n] - uintptr(e.Off)
			}
		}
		if len(d.Code) > 0 {
			if got := unsafe.Slice((*byte)(nativePtr(&own)), len(d.Code)); !bytes.Equal(got, d.Code) {
				t.Fatalf("the linked %s block differs from the generator's code", d.Name)
			}
		}
		for n, e := range d.Table {
			want := base + uintptr(e.Off)
			if e.Own {
				want = own + uintptr(e.Off)
			}
			if tab[n] != want {
				t.Fatalf("%s's table entry %d is %#x, want %#x", d.Name, n, tab[n], want)
			}
		}
	}
}

// TestRegNativePlainSameWithJIT checks that the plain dispatcher a build
// with the JIT makes -- the handlers' block, its table and their file --
// is the one a build without it makes: one file serves both, and the
// feedback dispatcher only adds to it.
func TestRegNativePlainSameWithJIT(t *testing.T) {
	without, with := nativeBuild(t), nativeBuild(t, "quickjs_jit")
	if without.Dispatchers[0].Name != "plain" || with.Dispatchers[0].Name != "plain" {
		t.Fatal("plain is not the first dispatcher")
	}
	if !bytes.Equal(without.Files[0].Data, with.Files[0].Data) {
		t.Fatalf("a build with the JIT makes another %s", with.Files[0].Name)
	}
	if !bytes.Equal(without.Dispatchers[0].Code, with.Dispatchers[0].Code) || !slices.Equal(without.Dispatchers[0].Table, with.Dispatchers[0].Table) {
		t.Fatal("a build with the JIT makes another plain dispatcher")
	}
	if len(with.Dispatchers) < 2 || with.Dispatchers[1].Name != "feedback" {
		t.Fatal("a build with the JIT makes no feedback dispatcher")
	}
	for _, d := range without.Dispatchers {
		if d.Name != "plain" {
			t.Fatalf("a build without the JIT makes the %s dispatcher", d.Name)
		}
	}
}
