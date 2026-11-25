package vm

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// intendedInlining is the functions of package vm the interpreter's hot
// paths call that must stay small enough for Go's inliner (a cost of 80):
// one that grows past it is a call where it was a few instructions, which
// no test fails and a benchmark shows only as a few per cent here and
// there. Each is inlinable in this package's builds with and without the
// JIT, and on main as of 2026-11 where main has it. Several had been lost
// to changes that grew them or what they call -- the prototype moved into
// the shape made Object.proto bigger, and plainArray, chainHasProxy,
// protoValue and ctorRoot, which call it, went over -- and were found only
// by diffing -gcflags=-m's "can inline" lines between builds.
var intendedInlining = []string{
	// What the register interpreter's handlers, callObject and runFD call
	// for every property, element and call.
	"(*Object).vals",
	"(*Object).valAt",
	"(*Object).keys",
	"(*Object).proto",
	"(*Object).layoutProto",
	"(*Object).cacheShape",
	"(*Object).elemsOf",
	"(*Object).extAt",
	"(*Object).dataOf",
	"(*Object).fn",
	"(*Object).shapeIndex",
	"(*Object).transition",
	"(*Object).IsExtensible",
	"plainOwn",
	"proxyOf",
	"isHole",
	"remember",
	"ownOf",
	"megaSet",
	"(*propCache).own",
	"(*propCache).holder",
	"(*Runtime).tick",
	"(*Runtime).jitOn",
	"Value.IsObject",
	"Value.Object",
	"Obj",
	"Float",
	// A construction's root (constructWithTarget, pureConstruct).
	"(*shapeTree).ctorRoot",
	"(*shapeTree).siteRoot",
	"(*shape).orRoot",
	"(*shape).roomFor",
	"(*shape).noteBuilt",
	"(*funcData).ctorShape",
	// Literal headers and values initialized together, and the gate of
	// the first literal's lookup of its shared root.
	"newLiteralPair",
	"(*Runtime).regLiteralKeyedRoot",
	// The array methods' receiver and element reads, Object.getPrototypeOf,
	// a global's read past a with or a proxy.
	"(*Runtime).plainArray",
	"(*Runtime).kept",
	"(*Runtime).ownText",
	"(*arrayLike).dense",
	"protoValue",
	"chainHasProxy",
	// A property's value appended where a property is added (appendProp,
	// appendTransition).
	"(*Object).putVal",
	// A WeakMap's key's entries, and an iterator's chain (Iterator.from).
	"(*Object).weakMapRefsOf",
	"(*Runtime).inheritsFrom",
	// The register loop's typed array elements, in get_index's and
	// set_index's cases: past the budget, each is a call with the pc
	// live across it.
	"regTypedAt",
	"regTypedStore",
	// The register interpreter's handlers' jump and truthiness, which
	// keep a compare-and-branch's handler a leaf, and the state a frame
	// takes, keeps its pc in and gives back.
	"regJump",
	"regTruth",
	"(*regState).at",
	"(*regState).done",
	"(*Runtime).regEnter",
	"(*Runtime).regLeave",
	// An addition's slow path, its call of add or of the mark's form
	// that calls add: inlined, the handler makes one call as it did
	// before there were marks.
	"(*regState).addSeen",
	"(*regState).stringSeen",
	"(*regState).addImmSeen",
	"(*regState).addPropSeen",
}

// TestIntendedInlining builds the package with -gcflags=-m, plain and with
// the quickjs_jit tag, and fails for each function of intendedInlining the
// compiler does not report it can inline, as Go's own
// cmd/compile/internal/test's TestIntendedInlining does. Each build compiles
// the package anew unless the build cache has it -- some 25 seconds for the
// two then, under one second from the cache -- so it is skipped under
// -short; CI's go test ./... steps run it.
func TestIntendedInlining(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the package twice with -gcflags=-m")
	}
	gotool := filepath.Join(runtime.GOROOT(), "bin", "go")
	if _, err := os.Stat(gotool); err != nil {
		if gotool, err = exec.LookPath("go"); err != nil {
			t.Skip("no go command")
		}
	}
	can := regexp.MustCompile(`: can inline (\S+)`)
	// The two builds run at once, beside the package's other parallel tests.
	t.Parallel()
	for _, tags := range []string{"", "quickjs_jit"} {
		t.Run("tags="+tags, func(t *testing.T) {
			t.Parallel()
			args := []string{"build", "-gcflags=github.com/go-quickjs/go-quickjs/internal/vm=-m"}
			if tags != "" {
				args = append(args, "-tags", tags)
			}
			cmd := exec.Command(gotool, append(args, ".")...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
			}
			inlinable := map[string]bool{}
			for _, m := range can.FindAllStringSubmatch(string(out), -1) {
				inlinable[m[1]] = true
			}
			for _, fn := range intendedInlining {
				if !inlinable[fn] {
					t.Errorf("%s is no longer inlinable (tags %q): it has grown past the inliner's budget, or calls what has", fn, tags)
				}
			}
		})
	}
}
