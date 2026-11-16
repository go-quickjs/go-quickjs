package nativegen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"sort"
	"strings"
	"unsafe"
)

// Layout is what the emitters read of package vm's private layout, as
// plain numbers by name: the offsets of the fields of the types the
// handlers read and write, by "Type.field"; the types' sizes; and the
// constants the handlers test values and states against. One layout
// serves every architecture of a word size, whose structs Go lays out
// alike: the helper says which architecture's build gave it, and its
// word size.
type Layout struct {
	GOARCH  string            `json:"goarch"`
	PtrSize int64             `json:"ptrSize"`
	Offsets map[string]int64  `json:"offsets"`
	Sizes   map[string]int64  `json:"sizes"`
	Consts  map[string]uint64 `json:"consts"`
	// JITBuilt says the build had the JIT (vm's jitBuilt), whose fields
	// the Runtime and a closure's code have in it alone.
	JITBuilt bool `json:"jitBuilt"`
}

// layoutTag is the build tag that alone builds vm's NativeLayout
// (zzzyn_native_layout.go) and the helper that prints it.
const layoutTag = "quickjs_nativelayout"

// helper is the helper's package, which imports vm.
const helper = "github.com/go-quickjs/go-quickjs/internal/vm/internal/nativegen/cmd/layout"

// wordSize is each target's word size, of the architectures whose
// handlers are generated.
var wordSize = map[string]int64{"amd64": 8, "arm64": 8}

// jitTarget says where a build with the quickjs_jit tag has the JIT, for
// a target architecture: amd64's on linux and windows, arm64's on darwin
// (vm's zzjit_*.go). Its layout has the JIT's fields.
var jitTarget = map[string]bool{"amd64": true, "arm64": true}

// ReadLayout reads vm's layout in a build for goarch with the build tags
// tags, by running the helper (go run, with vm's NativeLayout built in)
// on the host. Go lays vm's structs out alike on every architecture of a
// word size, so the host's own build gives the layout of any target of
// its word size, and the helper is built for the host's architecture,
// which the host runs: amd64's layout is read on an arm64 host as on an
// amd64 one, and checked to be of the target's word size. A host of
// another word size builds the helper for goarch, which it can run only
// where it emulates it.
//
// The one thing a host cannot give is a build's that its own GOOS and
// GOARCH leave files out of: the JIT's, built for linux and windows on
// amd64 and darwin on arm64, whose fields the Runtime has only there.
// Asked for a layout with the quickjs_jit tag, a host whose own build
// has no JIT -- linux on arm64, say -- fails, saying so, rather than give
// a layout without the JIT's fields.
func ReadLayout(goarch string, tags ...string) (*Layout, error) {
	size, ok := wordSize[goarch]
	if !ok {
		return nil, fmt.Errorf("nativegen: no layout for %s", goarch)
	}
	gotool, err := exec.LookPath("go")
	if err != nil {
		return nil, err
	}
	build := goarch
	if hostSize := int64(unsafe.Sizeof(uintptr(0))); hostSize == size {
		build = runtime.GOARCH
	}
	cmd := exec.Command(gotool, "run", "-tags", strings.Join(append([]string{layoutTag}, tags...), ","), helper)
	cmd.Env = append(os.Environ(), "GOARCH="+build)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("nativegen: reading vm's %s layout (tags %q) by a build for %s: %v\n%s", goarch, tags, build, err, stderr.Bytes())
	}
	l := new(Layout)
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.DisallowUnknownFields()
	if err := dec.Decode(l); err != nil {
		return nil, fmt.Errorf("nativegen: vm's %s layout (tags %q): %v", goarch, tags, err)
	}
	if l.PtrSize != size {
		return nil, fmt.Errorf("nativegen: vm's %s layout read by a build for %s, whose words are %d bytes, not %d", goarch, l.GOARCH, l.PtrSize, size)
	}
	if slices.Contains(tags, "quickjs_jit") && jitTarget[goarch] && !l.JITBuilt {
		return nil, fmt.Errorf("nativegen: vm's %s layout with the JIT cannot be read on %s/%s, whose build has no JIT: read it on linux or windows amd64, or darwin arm64", goarch, runtime.GOOS, l.GOARCH)
	}
	return l, nil
}

// reader looks a layout's numbers up and records what the emitters read,
// so that a name vm no longer has fails the generation rather than read
// as zero.
type reader struct {
	l      *Layout
	failed []string
}

// lay is the layout being generated from, which the emitters read by off
// and size and loadOffsets and loadConsts load: set only while Generate
// holds genMu.
var lay *reader

// off is field name's offset, "Type.field", in the layout being
// generated from; size is type name's size.
func off(name string) int32 { return lay.off(name) }

func size(name string) int32 { return lay.size(name) }

func (r *reader) miss(kind, name string) {
	r.failed = append(r.failed, kind+" "+name)
}

// off is field name's offset, "Type.field".
func (r *reader) off(name string) int32 {
	v, ok := r.l.Offsets[name]
	if !ok {
		r.miss("offset", name)
	}
	return int32(v)
}

// size is type name's size.
func (r *reader) size(name string) int32 {
	v, ok := r.l.Sizes[name]
	if !ok {
		r.miss("size", name)
	}
	return int32(v)
}

// konst is constant name.
func (r *reader) konst(name string) uint64 {
	v, ok := r.l.Consts[name]
	if !ok {
		r.miss("constant", name)
	}
	return v
}

func (r *reader) err() error {
	if len(r.failed) == 0 {
		return nil
	}
	sort.Strings(r.failed)
	return fmt.Errorf("nativegen: vm's layout has no %s (NativeLayout, zzzyn_native_layout.go)", strings.Join(r.failed, ", "))
}
