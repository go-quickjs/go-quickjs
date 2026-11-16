package nativegen

import (
	"bytes"
	"fmt"
	"go/build/constraint"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// File is a file the generator writes in internal/vm.
type File struct {
	Name string
	Data []byte
}

// block is an emitter's code for a variant: its bytes, where its exits
// and each handler start, and the operations in the code's order.
type block struct {
	code          []byte
	exit, exitRet int
	at            map[bytecode.ROp]int
	order         []bytecode.ROp
}

// entry is op's table entry, its handler's offset in b, the exit stub's
// where it has none.
func (b *block) entry(op bytecode.ROp) int {
	if o, ok := b.at[op]; ok {
		return o
	}
	return b.exit
}

// arch is an architecture's emitter and the files it writes.
type arch struct {
	goarch string
	// emit makes variant v's block from the layout loaded (lay).
	emit func(v *variant) (*block, error)
	// tableAddr is the assembly of a Go function, func name() uintptr,
	// that returns the address of symbol sym.
	tableAddr func(name, sym string) string
	// emitter and ctxReg name the emitter and the register the context is
	// in, for the files' comments; words says the assembler takes the
	// machine code as 32-bit words (WORD), not bytes.
	emitter, ctxReg string
	words           bool
}

// file is the name of v's file for ar: the handlers' file for plain.
func (ar *arch) file(v *variant) string {
	if v == plain {
		return "zzzyn_native_handlers_" + ar.goarch + ".s"
	}
	return "zzzyn_native_" + v.name + "_" + ar.goarch + ".s"
}

// arches are the architectures whose handlers are generated.
var arches = []*arch{
	{goarch: "amd64", emit: nativeEmit, tableAddr: tableAddrAMD64, emitter: "nativeEmit", ctxReg: "DI"},
	{goarch: "arm64", emit: armEmit, tableAddr: tableAddrARM64, emitter: "armEmit", ctxReg: "R25", words: true},
}

// genMu holds lay while an emitter runs.
var genMu sync.Mutex

// emitFrom runs ar's emitter on layout l for variant v.
func (ar *arch) emitFrom(l *Layout, v *variant) (*block, error) {
	genMu.Lock()
	defer genMu.Unlock()
	lay = &reader{l: l}
	defer func() { lay = nil }()
	// The constants first: the offsets relative to rt and cc are by
	// rtBase and ccBase.
	loadConsts()
	loadOffsets()
	if err := lay.err(); err != nil {
		return nil, err
	}
	emit := ar.emit
	if own := v.own[ar.goarch]; own != nil {
		emit = own
	}
	b, err := emit(v)
	if err != nil {
		return nil, fmt.Errorf("nativegen: %s: %v", ar.goarch, err)
	}
	if err := lay.err(); err != nil {
		return nil, err
	}
	if err := handlerOrder(ar.goarch, b.order); err != nil {
		return nil, err
	}
	return b, nil
}

// Dispatcher is a variant's dispatcher as generated: the variant's own
// block -- plain's, the handlers' block -- and its table, each entry an
// offset in that block or in plain's.
type Dispatcher struct {
	Name  string
	Code  []byte
	Table []Slot
}

// Output is what the generator makes for one architecture from the
// layout of one build: the dispatchers that build has, plain's first,
// and their files.
type Output struct {
	Files       []File
	Dispatchers []Dispatcher
}

func lookup(goarch string) (*arch, error) {
	for _, ar := range arches {
		if ar.goarch == goarch {
			return ar, nil
		}
	}
	return nil, fmt.Errorf("nativegen: no emitter for %s", goarch)
}

// Build generates goarch's dispatchers that a build for goarch on this
// host's GOOS with the tags tags has -- plain, and each variant whose
// file's constraint holds there -- from that build's layout (ReadLayout).
// A build none of whose variants needs the JIT has its layout read as it
// is, with the quickjs_jit tag or not: darwin's on amd64 has no JIT
// whatever its tags.
func Build(goarch string, tags ...string) (*Output, error) {
	ar, err := lookup(goarch)
	if err != nil {
		return nil, err
	}
	want := func(v *variant) bool { return v.in(goarch, tags) }
	needJIT := false
	for _, v := range variants {
		if v != plain && want(v) && slices.Contains(v.tags, "quickjs_jit") {
			needJIT = true
		}
	}
	l, err := readLayout(goarch, needJIT, tags)
	if err != nil {
		return nil, err
	}
	return ar.build(l, want)
}

// build makes the variants that want says, plain always, from layout l.
func (ar *arch) build(l *Layout, want func(*variant) bool) (*Output, error) {
	out := new(Output)
	var p *block
	for _, v := range variants {
		if v != plain && !want(v) {
			continue
		}
		b, err := ar.emitFrom(l, v)
		if err != nil {
			return nil, err
		}
		if v == plain {
			p = b
		}
		out.Dispatchers = append(out.Dispatchers, Dispatcher{Name: v.name, Code: b.code, Table: v.table(p, b)})
		out.Files = append(out.Files, File{Name: ar.file(v), Data: ar.source(v, p, b)})
	}
	return out, nil
}

// in reports whether a build for goarch, on this host's GOOS, with the
// tags tags, has v: whether its file's constraint holds.
func (v *variant) in(goarch string, tags []string) bool {
	if v.build == "" {
		return true
	}
	return holds(v.build, goarch, runtime.GOOS, tags)
}

// holds reports whether the build constraint expr holds in a build for
// goos and goarch with the tags tags.
func holds(expr, goarch, goos string, tags []string) bool {
	e, err := constraint.Parse("//go:build " + expr)
	if err != nil {
		panic(fmt.Sprintf("nativegen: constraint %q: %v", expr, err))
	}
	return e.Eval(func(tag string) bool {
		return tag == goarch || tag == goos || slices.Contains(tags, tag)
	})
}

// Generate makes every architecture's files, on any host that runs its
// programs and whose own build has the JIT: plain's from the layout of a
// build without tags, each other variant's from the layout of a build
// with its tags. plain's file, which every build has, must be what each
// variant's build makes of it too.
func Generate() ([]File, error) {
	files, unread, err := GenerateReadable()
	if err == nil && len(unread) > 0 {
		err = fmt.Errorf("nativegen: %s/%s cannot read the layouts of %s, whose builds have the JIT: generate on linux or windows amd64, or darwin or linux arm64", runtime.GOOS, runtime.GOARCH, strings.Join(unread, ", "))
	}
	return files, err
}

// GenerateReadable is Generate on a host that may not read every
// variant's layout -- one whose own build has no JIT, darwin's on amd64,
// cannot read a layout with the JIT's fields (ReadLayout) -- making
// every file it can and naming those it leaves out.
func GenerateReadable() (files []File, unread []string, err error) {
	layouts := map[string]*Layout{}
	for _, ar := range arches {
		var base *Output
		for _, v := range variants {
			needJIT := slices.Contains(v.tags, "quickjs_jit") && jitTarget[ar.goarch]
			if needJIT && !hostJIT(ar.goarch) {
				unread = append(unread, ar.file(v))
				continue
			}
			// The helper is built for the host where it can be, whose
			// layout serves every target of its word size: one read serves
			// them all.
			key := helperArch(ar.goarch) + "|" + strings.Join(v.tags, ",")
			l := layouts[key]
			if l == nil {
				if l, err = readLayout(ar.goarch, needJIT, v.tags); err != nil {
					return nil, nil, err
				}
				layouts[key] = l
			}
			out, err := ar.build(l, func(w *variant) bool { return w == v })
			if err != nil {
				return nil, nil, err
			}
			if v == plain {
				base = out
				files = append(files, out.Files[0])
				continue
			}
			if !bytes.Equal(out.Files[0].Data, base.Files[0].Data) {
				return nil, nil, fmt.Errorf("nativegen: %s: a build with tags %q makes another %s than a build without", ar.goarch, v.tags, base.Files[0].Name)
			}
			files = append(files, out.Files[1])
		}
	}
	return files, unread, nil
}
