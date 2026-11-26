// Command layoutpad keeps package vm's layout lock: dead padding, generated,
// that puts the hot code and the Go runtime's hot data at the addresses a
// set of recorded targets gives them, whatever a change does around them.
//
// Package vm's speed on AMD Zen 2 depends on where its hot code falls
// modulo 4 KB, which the op cache indexes it by, and the Go runtime's hot
// variables within their 64-byte cache lines; a change anywhere moves what
// comes after it, and micro-benchmarks running the same instructions have
// measured 3 to 10% slower at every placement for it. The lock pins a build's
// layout so that a change measures as its code: each pad absorbs what the
// code before it moved.
//
// The levers, each a pad in a file internal/vm sorts where its gap must
// open, its size a constant of the build's configuration
// (layoutpad_GOOS_GOARCH.go, layoutpad_jit_GOOS_GOARCH.go and the wasm
// configurations' files), all of them
// byte stores to a variable never read, 7 bytes of code a store, so that a
// pad fills any whole number of 32-byte slots:
//
//   - A function pad (layoutPadA ... layoutPadZ): it moves every function
//     after it in file order and, since every function comes before every
//     closure and generic, everything after.
//   - A generic pad (layoutPadGA ... layoutPadGZ) the function pad calls.
//     Go reads a package's function bodies last first, then its methods',
//     and instantiates a generic where a body first meets it: a generic one
//     file's function calls is laid out after those the files after it meet
//     first and before those of the files before it. It moves the generics
//     after it.
//   - A method's generic pad (layoutPadMA ... layoutPadMZ), which a method
//     of *Runtime at the file calls, the same among the generics methods
//     meet first. The method comes before the function pad in its file,
//     so that it moves nothing the function pad holds.
//   - An assembly pad (H, HT, HJ): see below.
//   - A generic closure pad (layoutPadCZ), a closure in a generic the last
//     file calls: the closures of generics come first among the generics.
//   - Data pads: 8-byte pointer variables (DH) and 8- and 16-byte variables
//     without pointers (DW, DG), which the linker sorts by size among the
//     zeroed data before the Go runtime's mheap_, writeBarrier and
//     gcController, and so moves within their cache lines.
//   - An outer pad (X, XJ), a function pad in a package linked before vm,
//     in the file that sorts first there (internal/regexp/aa_layoutpad.go,
//     internal/jit/aa_layoutpad.go),
//     its constant in that package's configuration files: it moves the
//     whole package, which a change growing the packages linked before it
//     moves as much as vm, and which vm's pads, after it, cannot reach.
//   - An assembly pad (H), a function of raw bytes in an assembly file
//     sorted before the native handlers' files
//     (native_a_layoutpad_*.s), its size a whole number of 32-byte
//     slots (HT, before the typed code's handlers, and HJ, before the
//     JIT's leaf stubs, are the same): package vm's assembly is laid out after every closure of its
//     named functions, so that any closure's growth moves the native
//     handlers' block (nativeEnter, nativeCallH and regNativeHandlers,
//     the feedback handlers after it), which no Go pad can reach without moving the
//     closures. The pad moves that block, and the package's init and the
//     closures of generics after it, which CZ absorbs.
//
// docs/optimizing.md ("The layout lock") says what each can and cannot move.
//
// The pads are never called and their variables never read: one init calls
// them behind a variable that is never true, which keeps the linker from
// dropping them. The constants are all that changes from one regeneration to
// the next, and the pads' data is the same size whatever they are but the
// data levers' own. A pad only grows: an anchor a change moves back by 32
// bytes takes a pad 4,064 bytes longer.
//
// A pad that so grows past what the code before its anchor shrank moves
// everything after it a whole page, 4 KB, against everything before it,
// every offset modulo 4 KB the same: the targets hold, and the hot code
// after it is a page further from the runtime and from the packages linked
// before vm, which Zen 2 still tells apart (c7c7830a took the lock's E so
// from none to 4,011 bytes, and json_stringify_decimals was 46% slower at
// every placement). So the targets file records each code anchor's
// address as well (@ADDR), and an anchor whose page -- its address over
// 4,096 -- is not the recorded one fails both a regeneration and -check,
// which name the first anchor that moved and the pad or the code before
// it that did. The fix is to move what grew out of that pad's range, after
// its anchor, as b5df63ea did; -pages records the new pages where the move
// is meant (code that grew by a page or more, where every pad after it
// moves the same).
//
// The targets (internal/vm/layoutlock.txt) give, for each configuration --
// GOOS, GOARCH, and plain, with the JIT (jit), with the WebAssembly engine
// (wasm) or with both (jit+wasm) -- each lever's anchor, the symbol right
// after its gap, and the address the anchor must have modulo 4,096 for code
// and 64 for data, in a build of internal/cmd/v8bench made as
//
//	CGO_ENABLED=0 GOAMD64=v1 go build -buildvcs=false [-tags quickjs_jit,quickjs_wasm] ./internal/cmd/v8bench
//
// A configuration's files are built only in its builds:
// layoutpad_GOOS_GOARCH.go plain, layoutpad_jit_..., layoutpad_wasm_...
// and layoutpad_jit_wasm_..., each constrained by both tags.
// with the toolchain the targets name; that is how the placements tool
// builds too. A lever without an anchor in a configuration is left out of
// its builds, and a generic pad no configuration anchors is not written.
// The targets are one toolchain's: the tool refuses another (-check exits 3,
// which TestLayoutLock takes as a skip), and other platforms build no pads
// (layoutpad_other.go).
//
//	go run ./internal/cmd/layoutpad             # pad every configuration to its targets
//	go run ./internal/cmd/layoutpad -check      # this machine's configurations: exit 1 on drift
//	go run ./internal/cmd/layoutpad -show       # where each pad and anchor is, and what follows each pad
//	go run ./internal/cmd/layoutpad -record     # set the targets to the tree's present layout
//	go run ./internal/cmd/layoutpad -pages      # pad, and record the anchors' pages as they are
//
// -record, run at a base revision with the tool built beforehand and the
// pad files moved out of the tree, takes the base's layout as the targets:
// the anchors named in the targets file (-targets FILE, outside the tree),
// at the addresses they have there.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
)

const (
	vmDir      = "internal/vm"
	targetsRel = "internal/vm/layoutlock.txt"
	vmPrefix   = "github.com/go-quickjs/go-quickjs/internal/vm."
	modPrefix  = "github.com/go-quickjs/go-quickjs/"
	// codeMod and dataMod are what a target is modulo: the op cache's 4 KB
	// for code, the cache line for data.
	codeMod = 4096
	dataMod = 64
	// slot is the alignment of every function on amd64.
	slot = 32
	// maxStores is the most byte stores a pad has, storeBits the bits of
	// its lever's constant, storesALine how many the source has on a line,
	// and padBytes the bytes they store to: two a store. A store is 7
	// bytes of code, and 4,096 bytes and a slot take 590; a pad may hold
	// 8 KB of reserve past a page's worth (reserve.go).
	maxStores   = 1<<storeBits - 1
	storeBits   = 11
	storesALine = 8
	padBytes    = 2 * (maxStores + 1)
	// perStore is a store's bytes of code, before a build measures it.
	perStore = 7
	// dataVars is how many variables each data lever has.
	dataVars = 8
)

// A position is where a function pad, its generic pad and its method's go.
type position struct {
	name string // the lever's letter
	file string // the file in internal/vm, sorted where the gap opens
	doc  string
}

var positions = []position{
	{"A", "aa_layoutpad.go", "before every file of package vm: the built-ins"},
	{"I", "bigi_layoutpad.go", "after atom.go, before bigint.go: the BigInts' arithmetic, bigParts and bigOp"},
	{"E", "builtin_layoutpad.go", "after bigown.go, before builtins.go: the built-ins' hot loops, firstElem"},
	{"T", "builtins_j_layoutpad.go", "after builtins_iterzip.go, before builtins_json.go: the JSON codec, its encoder's encode"},
	{"JE", "builtins_json_e_layoutpad.go", "after builtins_json.go, before builtins_json_elements.go: the JSON encoder after encodeObject, encodeElements first, and its strings' escaping, jsonEscapeWide"},
	{"JP", "builtins_json_p_layoutpad.go", "after builtins_json_elements.go, before builtins_json_parse.go: JSON.parse's parser, parseJSON first, up to parseValueAt"},
	{"JO", "builtins_json_parse_o_layoutpad.go", "after builtins_json_parse.go, before builtins_json_parse_object.go: the JSON parser after parseValueAt, parseObject first, its arrays, strings and numbers, and the built-ins after it up to BM"},
	{"BM", "builtins_ma_layoutpad.go", "after builtins_json_parse_object.go, before builtins_map.go: a Map's and a Set's lookups, (*jsMap).find"},
	{"TA", "builtins_ty_layoutpad.go", "after builtins_string.go, before builtins_typedarray.go: a typed array's element reads and writes, (*typedArrayData).getElem"},
	{"CB", "callb_layoutpad.go", "after byvalue.go, before callback.go: the array methods' calls of their callbacks, (*regCallback).call"},
	{"C", "calls_layoutpad.go", "after callbound.go, before callsite.go: the case conversion's memo, asciiCaseMemo"},
	{"D", "defg_layoutpad.go", "after deferred.go, before define.go: the files after the built-ins"},
	{"F", "generato_layoutpad.go", "after generate.go, before generator.go: an iterator's results, iterResult"},
	{"IN", "ins_layoutpad.go", "after init.go, before instanceof.go: instanceof, the Intl bindings and the iteration of a for-of (iterate.go), past New, which is longer with the JIT built in"},
	{"L", "vl_layoutpad.go", "after value.go, before vm.go: the arithmetic in Go, add and arith"},
	{"B", "vm_b_layoutpad.go", "after vm_amath.go, before vm_call.go: callObject, callDirect, runFD"},
	{"JB", "jit_bi_layoutpad.go", "after jit_bg.go, before jit_bigint.go: the JIT's BigInt leaves, jitBigLeafMake"},
	{"JU", "jit_bu_layoutpad.go", "after jit_bigtyped.go, before jit_builder.go: a string builder's appends, jitAppend"},
	{"J", "jit_layoutpad.go", "after jit_keyedwrite.go, before jit_lazyframes.go and jit_leaffast.go: the JIT's leaf calls, jitLeafGo"},
	{"JN", "jit_na_layoutpad.go", "after jit_loop.go, before jit_native.go: native code's entries and its calls of Go with a frame, jitRegWhole and jitRegGoCall"},
	{"JS", "jit_ss_layoutpad.go", "after jit_snapshot_gen.go, before jit_ssa.go: native code's calls of host functions, jitHostCall"},
	{"KO", "keyo_layoutpad.go", "after jit_virtual.go, the JIT's last file (jit_disabled.go without the JIT), before keyorder.go: what follows the JIT's files, reserveFunctionProtoKeys first, at the same offsets in every build -- a string's concatenation (memory.go's concat), the native leaves (native_leaf.go)"},
	{"CC", "mem_layoutpad.go", "after localecase.go, before memory.go: a string's concatenation, concat, past the memory meter's walk of the frames, whose frameAt with the JIT built in tests for frames a call of Go put off"},
	{"N", "native_r_layoutpad.go", "after native_predicate.go, before native_run.go: the native dispatcher's Go side, nativeRun first, and its calls of Go, nativeApply, nativeNew and nativeBuiltin"},
	{"BU", "native_run_b_layoutpad.go", "after native_run.go, before native_run_builtin.go: the native dispatcher's calls of built-ins, nativeBuiltin first, past what the JIT's hooks add to native_run.go"},
	{"CN", "native_run_n_layoutpad.go", "after native_run_builtin.go, before native_run_new.go: the native dispatcher's constructions and applies, nativeNew first, nativeApply, past what the JIT's hooks add to native_run_builtin.go"},
	{"P", "reg_cal_layoutpad.go", "after reg_caches.go, before reg_call.go: the register interpreter's calls, regCall"},
	{"Q", "reg_ru_layoutpad.go", "after reg_report.go, before reg_run.go: the register interpreter's loop, runReg"},
	{"W", "reg_run_a_layoutpad.go", "after reg_run.go, before reg_run_call.go: the register interpreter's calls after its loop, rhCallW first"},
	{"WE", "reg_run_call_e_layoutpad.go", "after reg_run_call.go, before reg_run_call_enter.go: the frames rhCallW enters, regEnterW, past rhCallW's test of the JIT's entry"},
	{"LR", "reg_run_call_l_layoutpad.go", "after reg_run_call_enter.go, before reg_run_call_leaf.go: a direct call's frame and the leaf calls, regDirect, regNative and regLeafRun, past regEnterW's test of the JIT's entry"},
	{"U", "reg_runr_layoutpad.go", "after reg_run_call.go, before reg_runs.go: the register interpreter's operations in Go, regBinImm first, regBinary"},
	{"Y", "reg_gene_layoutpad.go", "after reg_genalloc.go, before reg_generator.go: generators and async functions, regGenOf first, regAsyncStart and regAsyncRun"},
	{"V", "reg_sw_layoutpad.go", "after reg_slow.go, before reg_switch.go: the register handlers, newRegSwitchTable first, reg_table*.go, at 2f05d3fc's layout"},
	{"VT", "reg_tab_layoutpad.go", "after reg_switch_cold.go, before reg_table.go: the register handlers in Go, rsAdd"},
	{"K", "reg_table_c_layoutpad.go", "after reg_table.go, before reg_table_cold.go: the cold handlers, regColdInit first, rhAwait among them"},
	{"VA", "valu_layoutpad.go", "after typeof.go, before value.go: a value's tests and accessors where they are not inlined, Value.IsNumber"},
	{"O", "vm_o_layoutpad.go", "after vm_leaf_pure.go, before vm_object.go: what follows the literal constructors, newErrorObject first, vm_property.go and vm_shape.go"},
	{"PR", "vm_p_layoutpad.go", "after vm_object.go, before vm_property.go: a property's read, getProp"},
	{"SH", "vm_s_layoutpad.go", "after vm_property.go, before vm_shape.go: a shape's root, rootFor"},
	{"R", "regexp_a_layoutpad.go", "after reg_yieldstar.go, before regexp_abstract.go: a replace's loop, regExpReplaceIn and its templates"},
	{"S", "strh_layoutpad.go", "after strcut.go, before string.go: a string's builder, partsBuilder"},
	{"Z", "z_layoutpad.go", "after every file: the closures, and the generics the last files call"},
}

// An outer is a function pad in a package linked before package vm, in
// the file that sorts first there: it moves every function of that package
// after it, and what follows the package up to vm, whose own levers absorb
// it. A package is laid out where the packages linked before it end, so a
// change that grows one of those -- internal/bytecode's operations -- moves
// its hot code, which vm's pads, after it, cannot reach. Its constants are
// in that package's own configuration files, as vm's are in vm's.
type outer struct {
	name string // the lever's letter, as in the targets file
	dir  string // the package's directory
	pkg  string // the package's name
	doc  string
}

var outers = []outer{
	{"X", "internal/regexp", "regexp", "before every file of package regexp: the matcher's run and search"},
	{"XJ", "internal/jit", "jit", "before every file of package jit: the calls of Go native code makes, ExpandGoCall"},
}

// outerOf is the outer pad a lever is, if it is one.
func outerOf(name string) (outer, bool) {
	for _, o := range outers {
		if o.name == name {
			return o, true
		}
	}
	return outer{}, false
}

// vmLever says whether a lever's constant is package vm's.
func vmLever(name string) bool {
	_, ok := outerOf(name)
	return !ok
}

// An asmPad is an assembly pad: a function of raw bytes in an assembly
// file of package vm sorted where its gap opens. Package vm's assembly is
// laid out after every closure of its named functions, in file order, so
// that no Go pad reaches it without moving the closures.
type asmPad struct {
	name   string // the lever's letters
	prefix string // its files' names, before the configuration
	doc    string
}

var asmPads = []asmPad{
	{"HJ", "jit_l_layoutpad_", "after the JIT's BigInt leaves' assembly, before jit_leaffast_<arch>.s: the JIT's leaf and Go call stubs, jitLeafFast and jitGoFast"},
	{"H", "native_a_layoutpad_", "after the JIT's assembly, before the native handlers' block: regNativeHandlers"},
	{"HT", "native_ta_layoutpad_", "after the native handlers' block, before native_typed_<arch>.s: the typed code's handlers, regNativeTypedHandlers"},
}

// asmPadOf is the assembly pad a lever is, if it is one.
func asmPadOf(name string) (asmPad, bool) {
	for _, a := range asmPads {
		if a.name == name {
			return a, true
		}
	}
	return asmPad{}, false
}

// A lever is one constant of a configuration's file.
type lever struct {
	name string // as in the targets file: A..Z, GA..GZ, MA..MZ, CZ, H, DH, DW, DG
	kind byte   // 'f' function pad, 'g' generic pad, 'c' generic closure pad, 'a' assembly pad, 'd' data
	pad  string // the pad's symbol, for code levers
	max  int    // the most the constant may be, for data levers
}

func levers() []lever {
	var ls []lever
	for _, p := range positions {
		ls = append(ls, lever{name: p.name, kind: 'f', pad: vmPrefix + "layoutPad" + p.name})
		ls = append(ls, lever{name: "G" + p.name, kind: 'g', pad: vmPrefix + "layoutPadG" + p.name + "[go.shape.struct {}]"})
		ls = append(ls, lever{name: "M" + p.name, kind: 'g', pad: vmPrefix + "layoutPadM" + p.name + "[go.shape.struct {}]"})
	}
	ls = append(ls, lever{name: "CZ", kind: 'c', pad: vmPrefix + "layoutPadCZ[go.shape.struct {}].func1"})
	for _, o := range outers {
		ls = append(ls, lever{name: o.name, kind: 'f', pad: modPrefix + o.dir + ".layoutPad" + o.name})
	}
	// An assembly function's symbol is its ABI0 one.
	for _, a := range asmPads {
		ls = append(ls, lever{name: a.name, kind: 'a', pad: vmPrefix + "layoutPad" + a.name + ".abi0"})
	}
	ls = append(ls,
		lever{name: "DH", kind: 'd', max: dataVars},
		lever{name: "DW", kind: 'd', max: dataVars},
		lever{name: "DG", kind: 'd', max: dataVars / 2})
	return ls
}

func leverByName(name string) (lever, bool) {
	for _, l := range levers() {
		if l.name == name {
			return l, true
		}
	}
	return lever{}, false
}

// A config is one build the targets pin: plain, with the JIT, with the
// WebAssembly engine (v8bench's quickjs_wasm build, which installs its
// namespace as qjs does), or with both.
type config struct {
	goos, goarch string
	jit, wasm    bool
	anchors      []anchor
}

// kinds are the configurations' names in the targets file, by jit and wasm.
var kinds = map[[2]bool]string{
	{false, false}: "plain",
	{true, false}:  "jit",
	{false, true}:  "wasm",
	{true, true}:   "jit+wasm",
}

func (c *config) kind() string { return kinds[[2]bool{c.jit, c.wasm}] }

func (c *config) String() string { return c.goos + "/" + c.goarch + " " + c.kind() }

// tags is the build tags of c's builds, for go build's -tags.
func (c *config) tags() string {
	var t []string
	if c.jit {
		t = append(t, "quickjs_jit")
	}
	if c.wasm {
		t = append(t, "quickjs_wasm")
	}
	return strings.Join(t, ",")
}

// buildLine is the build constraint of c's generated files, which no other
// configuration of the platform builds.
func (c *config) buildLine() string {
	jit, wasm := "quickjs_jit", "quickjs_wasm"
	if !c.jit {
		jit = "!" + jit
	}
	if !c.wasm {
		wasm = "!" + wasm
	}
	return "//go:build " + jit + " && " + wasm + "\n\n"
}

// filePrefix is what c's files are named with before the platform:
// layoutpad_jit_wasm_windows_amd64.go, say.
func (c *config) filePrefix() string {
	p := ""
	if c.jit {
		p += "jit_"
	}
	if c.wasm {
		p += "wasm_"
	}
	return p
}

// An anchor is the symbol a lever puts at its target. A code anchor's at is
// its address as the targets file records it, 0 for none: its page, at over
// codeMod, is the one every build must keep.
type anchor struct {
	lever  string
	symbol string
	target uint64
	at     uint64
	// fresh is an anchor the targets file names as new: the next
	// regeneration records it where its pad, at its least, leaves it.
	fresh bool
}

type targets struct {
	toolchain string
	configs   []*config
}

func main() {
	check := flag.Bool("check", false, "build this machine's configurations and report anchors away from their targets; exit 1 if any is")
	show := flag.Bool("show", false, "print each pad, the symbol after it and each anchor, for this machine's configurations")
	record := flag.Bool("record", false, "set every configuration's targets to where its anchors are in the tree as it is")
	only := flag.String("config", "", "only the configurations named, comma-separated, as GOOS/GOARCH/KIND, KIND plain, jit, wasm or jit+wasm")
	verbose := flag.Bool("v", false, "print each build's anchors away from their targets and what each lever is set to")
	targetsPath := flag.String("targets", targetsRel, "the targets file, from the repository's root")
	generics := flag.Bool("generics", false, "with -show -all, write every generic pad as well")
	all := flag.Bool("all", false, "with -show, write the pads and set every lever of the configurations to 0 first, to see where each lands")
	recordPages := flag.Bool("pages", false, "pad as by default, and record each code anchor's address (its page) as the build has it, where an anchor moved a page and the move is meant")
	hot := flag.Bool("hot", false, "profile this machine's configurations' micro-benchmarks and list the hot functions that lie far past their range's anchor, each wanting a position (docs/optimizing.md)")
	hotRuns := flag.Int("hotruns", 10, "with -hot, the micro-benchmarks' runs")
	hotShare := flag.Float64("hotshare", 0.002, "with -hot, the share of the samples that makes a function hot")
	hotDist := flag.Int64("hotdist", 8192, "with -hot, the bytes past its range's anchor that a hot function may lie")
	reserve := flag.String("reserve", "", "with -pages, the levers, comma-separated, whose pads take their reserve now, their anchors and every one after them recorded as many pages on as that needs (XJ,CZ)")
	recordEq := flag.Bool("eq", false, "pad as by default, and record the builds' equality functions (type:.eq.*) in "+eqRel+", where a new one is meant")
	flag.Parse()
	root, err := repoRoot()
	if err == nil {
		err = os.Chdir(root)
	}
	if err != nil {
		fatal(err)
	}
	t, err := readTargets(*targetsPath)
	if err != nil {
		fatal(err)
	}
	eqs, err := readEq(eqRel)
	if err != nil {
		fatal(err)
	}
	if v := goVersion(); v != t.toolchain && !*record {
		msg := fmt.Sprintf("the targets are %s's layout and this is %s: run with GOTOOLCHAIN=%s, or -record new targets", t.toolchain, v, t.toolchain)
		if *check {
			fmt.Println("layoutpad: skipped:", msg)
			os.Exit(3)
		}
		fatal(errors.New(msg))
	}
	var picked []*config
	for _, c := range t.configs {
		if *only != "" {
			name := c.goos + "/" + c.goarch + "/" + c.kind()
			if !slices.Contains(strings.Split(*only, ","), name) {
				continue
			}
		} else if (*check || *show || *hot) && (c.goos != runtime.GOOS || c.goarch != runtime.GOARCH) {
			continue
		}
		picked = append(picked, c)
	}
	if len(picked) == 0 {
		if *check {
			fmt.Printf("layoutpad: skipped: no targets for %s/%s\n", runtime.GOOS, runtime.GOARCH)
			os.Exit(3)
		}
		fatal(errors.New("no configuration picked"))
	}
	switch {
	case *hot:
		found := false
		for _, c := range picked {
			list, err := hotRun(c, *hotRuns, *hotShare, *hotDist)
			if err != nil {
				fatal(err)
			}
			for _, h := range list {
				// A closure is laid out among the closures, which no
				// position reaches (docs/optimizing.md): it is listed,
				// and fails nothing.
				closure := strings.Contains(h.name[strings.LastIndex(h.name, ")")+1:], ".func")
				note := ""
				if closure {
					note = ": a closure, which no position holds"
				}
				fmt.Printf("%s: %s (%s), %.2fs (%.1f%%), %d bytes past anchor %s%s\n", c, strings.TrimPrefix(h.name, modPrefix), h.file, h.seconds, 100*h.share, h.past, h.lever, note)
				found = found || !closure
			}
			if len(list) == 0 {
				fmt.Printf("%s: no hot function more than %d bytes past its range's anchor\n", c, *hotDist)
			}
		}
		if found {
			os.Exit(1)
		}
	case *check:
		drift, paged, eqFailed := false, false, false
		for _, c := range picked {
			syms, err := build(c)
			if err != nil {
				fatal(err)
			}
			lines := compare(c, syms)
			for _, line := range lines {
				fmt.Printf("%s: %s\n", c, line)
			}
			moved := pageDrift(c, syms)
			for _, line := range moved {
				fmt.Printf("%s: %s\n", c, line)
			}
			eqLines, eqNew := eqCheck(c, syms, eqs)
			for _, line := range eqLines {
				fmt.Printf("%s: %s\n", c, line)
			}
			if len(lines)+len(moved) == 0 && !eqNew {
				fmt.Printf("%s: every anchor at its target, on its page, and no new equality function\n", c)
			}
			drift = drift || len(lines) > 0
			paged = paged || len(moved) > 0
			eqFailed = eqFailed || eqNew
		}
		if drift {
			fmt.Println("layoutpad: the layout has drifted from its targets: run go generate ./internal/vm (go run ./internal/cmd/layoutpad)")
		}
		if paged {
			fmt.Println(pageAdvice)
		}
		if eqFailed {
			fmt.Println(eqAdvice)
		}
		if drift || paged || eqFailed {
			os.Exit(1)
		}
	case *show:
		if *all {
			everyGeneric = *generics
			if err := writeSources(t); err != nil {
				fatal(err)
			}
			for _, c := range picked {
				v := readValues(c)
				for _, l := range levers() {
					v[l.name] = 0
					if l.kind == 'a' {
						// The assembly pad's least: one slot.
						v[l.name] = 1
					}
				}
				if err := writeValues(c, v); err != nil {
					fatal(err)
				}
			}
		}
		for _, c := range picked {
			syms, err := build(c)
			if err != nil {
				fatal(err)
			}
			showLayout(c, syms)
		}
	case *record:
		for _, c := range picked {
			syms, err := build(c)
			if err != nil {
				fatal(err)
			}
			for i, a := range c.anchors {
				s, ok := syms.byName[a.symbol]
				if !ok {
					fatal(fmt.Errorf("%s: anchor %s is not in the build", c, a.symbol))
				}
				c.anchors[i].target = s.addr % modOf(a.lever)
				c.anchors[i].at = 0
				if modOf(a.lever) == codeMod {
					c.anchors[i].at = s.addr
				}
			}
			eqs[c.String()] = eqOf(syms)
			fmt.Printf("%s: recorded\n", c)
		}
		t.toolchain = goVersion()
		if err := writeTargets(*targetsPath, t); err != nil {
			fatal(err)
		}
		if err := writeEq(eqRel, t, eqs); err != nil {
			fatal(err)
		}
	default:
		if err := writeSources(t); err != nil {
			fatal(err)
		}
		paged, freshRecorded, eqFailed := false, false, false
		for _, c := range picked {
			syms, err := solve(c, *verbose)
			if err != nil {
				fatal(err)
			}
			if recordFresh(c, syms) {
				freshRecorded = true
			}
			if *recordEq {
				eqs[c.String()] = eqOf(syms)
			} else {
				lines, failed := eqCheck(c, syms, eqs)
				for _, line := range lines {
					fmt.Printf("%s: %s\n", c, line)
				}
				eqFailed = eqFailed || failed
			}
			if *recordPages {
				forced := map[string]bool{}
				for _, name := range strings.Split(*reserve, ",") {
					forced[name] = name != ""
				}
				if reservePages(c, syms, readValues(c), forced) {
					if syms, err = solve(c, *verbose); err != nil {
						fatal(err)
					}
				}
				setPages(c, syms)
				continue
			}
			for _, line := range pageDrift(c, syms) {
				fmt.Printf("%s: %s\n", c, line)
				paged = true
			}
		}
		if *recordPages || freshRecorded {
			if err := writeTargets(*targetsPath, t); err != nil {
				fatal(err)
			}
		}
		if *recordPages {
			fmt.Println("layoutpad: recorded the anchors' pages in", *targetsPath)
		} else if freshRecorded {
			fmt.Println("layoutpad: recorded the new anchors in", *targetsPath)
		}
		if *recordEq {
			if err := writeEq(eqRel, t, eqs); err != nil {
				fatal(err)
			}
			fmt.Println("layoutpad: recorded the equality functions in", eqRel)
		}
		if paged {
			fmt.Println(pageAdvice)
		}
		if eqFailed {
			fmt.Println(eqAdvice)
		}
		if paged || eqFailed {
			os.Exit(1)
		}
	}
}

// pageAdvice is what to do about an anchor a page off.
const pageAdvice = "layoutpad: an anchor is a page from where " + targetsRel + " records it, every offset modulo 4 KB the same: " +
	"move what grew before it out of the pad's range, after its anchor (docs/optimizing.md, \"The layout lock\"), " +
	"or, where the move is meant, record the new pages with go run ./internal/cmd/layoutpad -pages"

// recordFresh records each of c's new anchors where syms has it, its pad
// at its least, and says whether there was one.
func recordFresh(c *config, syms *symtab) bool {
	found := false
	for i, a := range c.anchors {
		if !a.fresh {
			continue
		}
		s, ok := syms.byName[a.symbol]
		if !ok {
			fatal(fmt.Errorf("%s: anchor %s is not in the build", c, a.symbol))
		}
		c.anchors[i].target, c.anchors[i].at, c.anchors[i].fresh = s.addr%codeMod, s.addr, false
		fmt.Printf("%s: anchor %s recorded at %#x\n", c, a.lever, s.addr)
		found = true
	}
	return found
}

// setPages records each code anchor's address in syms as its page.
func setPages(c *config, syms *symtab) {
	for i, a := range c.anchors {
		if modOf(a.lever) != codeMod {
			continue
		}
		if s, ok := syms.byName[a.symbol]; ok {
			c.anchors[i].at = s.addr
		}
	}
}

// pageDrift lists the anchors that are not on the page the targets record
// for them, each where a move starts: the first anchor, in address order,
// that moved, and any after it that moved by another count of pages. An
// anchor that moved as the one before it did moved with it.
func pageDrift(c *config, syms *symtab) []string {
	type placed struct {
		a     anchor
		addr  uint64
		pages int64
	}
	var list []placed
	for _, a := range c.anchors {
		if modOf(a.lever) != codeMod || a.at == 0 {
			continue
		}
		s, ok := syms.byName[a.symbol]
		if !ok || s.addr%codeMod != a.target {
			continue // compare says so
		}
		list = append(list, placed{a, s.addr, int64(s.addr/codeMod) - int64(a.at/codeMod)})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].addr < list[j].addr })
	var out []string
	for i, p := range list {
		if p.pages == 0 || i > 0 && list[i-1].pages == p.pages {
			continue
		}
		what := fmt.Sprintf("%d pages further from", p.pages)
		switch {
		case p.pages == 1:
			what = "a page further from"
		case p.pages == -1:
			what = "a page nearer"
		case p.pages < 0:
			what = fmt.Sprintf("%d pages nearer", -p.pages)
		}
		line := fmt.Sprintf("anchor %s (%s) is at %#x, %s the start of the text than %s records (%#x)",
			p.a.lever, p.a.symbol, p.addr, what, targetsRel, p.a.at)
		if i+1 < len(list) && list[i+1].pages == p.pages {
			line += ", and the anchors after it moved with it"
		}
		pad := "its pad"
		if l, ok := leverByName(p.a.lever); ok {
			if s, ok := syms.byName[l.pad]; ok {
				pad = fmt.Sprintf("its pad %s (%s, %d bytes)", l.name, strings.TrimPrefix(l.pad, vmPrefix), s.size)
			}
		}
		if i == 0 {
			line += fmt.Sprintf(": %s or the code before it (%s) -- and what is linked before -- grew past a page", pad, leverDoc(p.a.lever))
		} else {
			prev := list[i-1]
			was := "is where it was"
			if prev.pages != 0 {
				was = fmt.Sprintf("moved by %d", prev.pages)
			}
			line += fmt.Sprintf(": %s or the code in its range (%s) grew past a page, while anchor %s (%s) before it %s",
				pad, leverDoc(p.a.lever), prev.a.lever, prev.a.symbol, was)
		}
		out = append(out, line)
	}
	return out
}

// leverDoc says where a lever's pad is and what its anchor holds.
func leverDoc(name string) string {
	for _, p := range positions {
		if p.name == name || "G"+p.name == name || "M"+p.name == name {
			return p.file + ", " + p.doc
		}
	}
	if o, ok := outerOf(name); ok {
		return o.dir + "'s first file, " + o.doc
	}
	if a, ok := asmPadOf(name); ok {
		return "package vm's assembly, " + a.doc
	}
	switch name {
	case "CZ":
		return "the generics' closures, after the native handlers' block"
	}
	return "lever " + name
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "layoutpad:", err)
	os.Exit(2)
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil && isModule(b) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("not inside the go-quickjs module")
		}
		dir = parent
	}
}

// isModule says whether a go.mod is go-quickjs's, whatever its line endings:
// a checkout on Windows may have CRLF.
func isModule(gomod []byte) bool {
	for _, line := range strings.Split(string(gomod), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "module" {
			return f[1] == "github.com/go-quickjs/go-quickjs"
		}
	}
	return false
}

func goVersion() string {
	out, err := exec.Command("go", "env", "GOVERSION").Output()
	if err != nil {
		fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func modOf(name string) uint64 {
	if l, _ := leverByName(name); l.kind == 'd' {
		return dataMod
	}
	return codeMod
}

// readTargets reads the targets file: a toolchain line, then for each
// configuration a config line and its anchors.
func readTargets(path string) (*targets, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	t := &targets{}
	var cur *config
	for n, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		bad := func() error { return fmt.Errorf("%s:%d: cannot read %q", path, n+1, line) }
		switch f[0] {
		case "toolchain":
			if len(f) != 2 {
				return nil, bad()
			}
			t.toolchain = f[1]
		case "config":
			if len(f) != 3 {
				return nil, bad()
			}
			goos, goarch, ok := strings.Cut(f[1], "/")
			if !ok {
				return nil, bad()
			}
			cur = &config{goos: goos, goarch: goarch}
			known := false
			for k, name := range kinds {
				if name == f[2] {
					cur.jit, cur.wasm, known = k[0], k[1], true
				}
			}
			if !known {
				return nil, bad()
			}
			t.configs = append(t.configs, cur)
		case "anchor":
			// anchor LEVER TARGET [@ADDR] SYMBOL..., the symbol last: it may
			// have spaces.
			if cur == nil || len(f) < 4 {
				return nil, bad()
			}
			var at uint64
			if strings.HasPrefix(f[3], "@") {
				a, err := strconv.ParseUint(f[3][1:], 0, 64)
				if err != nil || len(f) < 5 {
					return nil, bad()
				}
				at, f = a, append(f[:3:3], f[4:]...)
			}
			if _, ok := leverByName(f[1]); !ok {
				return nil, fmt.Errorf("%s:%d: no lever %s", path, n+1, f[1])
			}
			fresh := f[2] == "new"
			var v uint64
			if !fresh {
				if v, err = strconv.ParseUint(f[2], 0, 64); err != nil {
					return nil, bad()
				}
			} else if at != 0 || modOf(f[1]) != codeMod {
				return nil, bad()
			}
			sym := strings.Join(f[3:], " ")
			if at != 0 && (modOf(f[1]) != codeMod || at%codeMod != v) {
				return nil, fmt.Errorf("%s:%d: %s's recorded address %#x is not at its target %#x", path, n+1, sym, at, v)
			}
			cur.anchors = append(cur.anchors, anchor{lever: f[1], symbol: sym, target: v, at: at, fresh: fresh})
		default:
			return nil, bad()
		}
	}
	if t.toolchain == "" {
		return nil, fmt.Errorf("%s names no toolchain", path)
	}
	return t, nil
}

// writeTargets rewrites the targets file's values, keeping its comments.
func writeTargets(path string, t *targets) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var out strings.Builder
	ci, ai := -1, 0
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) > 0 && f[0] == "toolchain":
			line = "toolchain " + t.toolchain
		case len(f) > 0 && f[0] == "config":
			ci, ai = ci+1, 0
		case len(f) > 0 && f[0] == "anchor":
			a := t.configs[ci].anchors[ai]
			ai++
			width := 3
			if modOf(a.lever) == codeMod {
				width = 5
			}
			if a.fresh {
				line = fmt.Sprintf("anchor %-2s new %s", a.lever, a.symbol)
			} else if a.at != 0 {
				line = fmt.Sprintf("anchor %-2s %#0*x @%#x %s", a.lever, width, a.target, a.at, a.symbol)
			} else {
				line = fmt.Sprintf("anchor %-2s %#0*x %s", a.lever, width, a.target, a.symbol)
			}
		}
		out.WriteString(line + "\n")
	}
	return os.WriteFile(path, []byte(out.String()), 0o644)
}

// A symbol is one line of go tool nm -size.
type symbol struct {
	addr, size uint64
	kind       string
	name       string
}

type symtab struct {
	list   []symbol // by address
	byName map[string]symbol
}

// build builds v8bench for c as its targets are measured and reads its
// symbols.
func build(c *config) (*symtab, error) {
	dir, err := os.MkdirTemp("", "layoutpad")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	exe, err := buildExe(c, dir)
	if err != nil {
		return nil, err
	}
	out, err := exec.Command("go", "tool", "nm", "-size", "-sort", "address", exe).Output()
	if err != nil {
		return nil, fmt.Errorf("go tool nm: %w", err)
	}
	return parseNM(out), nil
}

// buildExe builds v8bench for c in dir as its targets are measured, and
// returns the binary's path.
func buildExe(c *config, dir string) (string, error) {
	exe := filepath.Join(dir, "v8bench.exe")
	args := []string{"build", "-buildvcs=false", "-o", exe}
	if tags := c.tags(); tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, "./internal/cmd/v8bench")
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), "GOOS="+c.goos, "GOARCH="+c.goarch, "CGO_ENABLED=0", "GOAMD64=v1", "GOFLAGS=", "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("%s: go %s: %v\n%s", c, strings.Join(args, " "), err, out)
	}
	return exe, nil
}

func parseNM(out []byte) *symtab {
	t := &symtab{byName: map[string]symbol{}}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		f := strings.SplitN(strings.TrimSpace(sc.Text()), " ", 4)
		if len(f) < 4 {
			continue
		}
		// The size is padded with spaces: split again past it.
		rest := strings.TrimSpace(strings.Join(f[1:], " "))
		g := strings.Fields(rest)
		if len(g) < 3 {
			continue
		}
		addr, err1 := strconv.ParseUint(f[0], 16, 64)
		size, err2 := strconv.ParseUint(g[0], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(rest, g[0])), g[1]))
		s := symbol{addr, size, g[1], name}
		t.list = append(t.list, s)
		if _, dup := t.byName[name]; !dup {
			t.byName[name] = s
		}
	}
	sort.SliceStable(t.list, func(i, j int) bool { return t.list[i].addr < t.list[j].addr })
	return t
}

// compare lists each anchor away from its target.
func compare(c *config, syms *symtab) []string {
	var out []string
	for _, a := range c.anchors {
		s, ok := syms.byName[a.symbol]
		if !ok {
			out = append(out, fmt.Sprintf("anchor %s (%s) is not in the build: renamed? name the anchor anew in %s", a.lever, a.symbol, targetsRel))
			continue
		}
		if a.fresh {
			out = append(out, fmt.Sprintf("anchor %s (%s) is new: run go generate ./internal/vm to record it", a.lever, a.symbol))
			continue
		}
		m := modOf(a.lever)
		if got := s.addr % m; got != a.target {
			out = append(out, fmt.Sprintf("%s at %#x mod %d, its target %#x (lever %s)", a.symbol, got, m, a.target, a.lever))
		}
	}
	return out
}

// next is the first symbol after addr in the text, other than the pads.
func (t *symtab) next(addr uint64) (symbol, bool) {
	i := sort.Search(len(t.list), func(i int) bool { return t.list[i].addr > addr })
	for ; i < len(t.list); i++ {
		s := t.list[i]
		if (s.kind == "T" || s.kind == "t") && !strings.Contains(s.name, ".layoutPad") {
			return s, true
		}
	}
	return symbol{}, false
}

func showLayout(c *config, syms *symtab) {
	fmt.Printf("%s:\n", c)
	for _, l := range levers() {
		if l.kind == 'd' {
			continue
		}
		p, ok := syms.byName[l.pad]
		if !ok {
			fmt.Printf("  %-2s (not in the build)\n", l.name)
			continue
		}
		n, _ := syms.next(p.addr)
		fmt.Printf("  %-2s pad at %#x, %5d bytes; then %s at %#x\n", l.name, p.addr, p.size, n.name, n.addr%codeMod)
	}
	for _, a := range c.anchors {
		if s, ok := syms.byName[a.symbol]; ok {
			fmt.Printf("  anchor %-2s %-60s %#x (target %#x)\n", a.lever, a.symbol, s.addr%modOf(a.lever), a.target)
		} else {
			fmt.Printf("  anchor %-2s %-60s missing\n", a.lever, a.symbol)
		}
	}
}

// The configuration's file: its levers' constants.

func configFile(c *config) string { return configFileIn(c, vmDir) }

// configFileIn is the configuration's file in the package at dir.
func configFileIn(c *config, dir string) string {
	return filepath.Join(dir, "layoutpad_"+c.filePrefix()+c.goos+"_"+c.goarch+".go")
}

// configFiles are c's files: package vm's, then the outer pads', then the
// assembly pads'.
func configFiles(c *config) []string {
	files := []string{configFile(c)}
	for _, o := range outers {
		files = append(files, configFileIn(c, o.dir))
	}
	for _, a := range asmPads {
		files = append(files, asmFile(c, a))
	}
	return files
}

// asmFile is c's file of the assembly pad a, named to sort where its gap
// opens: H's after the JIT's assembly and before every native handlers'
// file (native_<arch>.s first), HJ's before the JIT's leaf stubs.
func asmFile(c *config, a asmPad) string {
	return filepath.Join(vmDir, a.prefix+c.filePrefix()+c.goos+"_"+c.goarch+".s")
}

// asmSource is c's assembly pad a of n 32-byte slots, int3 bytes, or a
// file with no pad where n is below 1, the lever left out.
func asmSource(c *config, a asmPad, n int) (string, error) {
	var b strings.Builder
	b.WriteString("// Code generated by internal/cmd/layoutpad; DO NOT EDIT.\n\n")
	b.WriteString(c.buildLine())
	if n < 1 {
		fmt.Fprintf(&b, "// The layout lock's assembly pad %s is left out for %s.\n", a.name, c)
		return b.String(), nil
	}
	if c.goarch != "amd64" {
		return "", fmt.Errorf("%s: the assembly pad %s is written for amd64 alone", c, a.name)
	}
	fmt.Fprintf(&b, "#include \"textflag.h\"\n\n")
	fmt.Fprintf(&b, "// layoutPad%s is the layout lock's assembly pad %s for %s:\n// %d slots of %d bytes (layoutlock.txt). It is never called: see\n// internal/cmd/layoutpad.\n", a.name, a.name, c, n, slot)
	fmt.Fprintf(&b, "TEXT ·layoutPad%s(SB), NOSPLIT|NOFRAME, $0-0\n", a.name)
	for range n {
		b.WriteString("\tQUAD $0xcccccccccccccccc; QUAD $0xcccccccccccccccc; QUAD $0xcccccccccccccccc; QUAD $0xcccccccccccccccc\n")
	}
	return b.String(), nil
}

// writeFile writes src to path unless it holds it already.
func writeFile(path, src string) error {
	if old, err := os.ReadFile(path); err == nil && string(old) == src {
		return nil
	}
	return os.WriteFile(path, []byte(src), 0o644)
}

// readValues reads a configuration's constants, -1 for a lever its file
// does not have yet.
func readValues(c *config) map[string]int {
	v := map[string]int{}
	for _, l := range levers() {
		v[l.name] = -1
		if l.kind == 'd' {
			v[l.name] = 0
		}
	}
	for _, file := range configFiles(c) {
		b, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "const "))
			if len(f) == 3 && f[1] == "=" && strings.HasPrefix(f[0], "layoutPadN") {
				if n, err := strconv.Atoi(f[2]); err == nil {
					v[strings.TrimPrefix(f[0], "layoutPadN")] = n
				}
			}
		}
	}
	return v
}

func writeValues(c *config, v map[string]int) error {
	var b strings.Builder
	b.WriteString("// Code generated by internal/cmd/layoutpad; DO NOT EDIT.\n\n")
	b.WriteString(c.buildLine())
	fmt.Fprintf(&b, "package vm\n\n// The layout lock's levers for %s (layoutlock.txt): a pad's byte\n// stores (the assembly pad's 32-byte slots), -1 for a pad left out, or a\n// data lever's variables.\nconst (\n", c)
	for _, l := range levers() {
		if vmLever(l.name) {
			fmt.Fprintf(&b, "\tlayoutPadN%s = %d\n", l.name, v[l.name])
		}
	}
	b.WriteString(")\n")
	for _, a := range asmPads {
		fmt.Fprintf(&b, "\n// layoutPad%s is an assembly pad, in\n// %s. It is never called.\nfunc layoutPad%s()\n", a.name, filepath.Base(asmFile(c, a)), a.name)
	}
	if err := writeGo(configFile(c), b.String()); err != nil {
		return err
	}
	for _, a := range asmPads {
		asm, err := asmSource(c, a, v[a.name])
		if err != nil {
			return err
		}
		if err := writeFile(asmFile(c, a), asm); err != nil {
			return err
		}
	}
	for _, o := range outers {
		b.Reset()
		b.WriteString("// Code generated by internal/cmd/layoutpad; DO NOT EDIT.\n\n")
		b.WriteString(c.buildLine())
		fmt.Fprintf(&b, "package %s\n\n// The layout lock's lever for %s (internal/vm/layoutlock.txt): its\n// pad's byte stores, -1 for the pad left out.\nconst layoutPadN%s = %d\n", o.pkg, c, o.name, v[o.name])
		if err := writeGo(configFileIn(c, o.dir), b.String()); err != nil {
			return err
		}
	}
	return nil
}

func writeGo(path, src string) error {
	formatted, err := format.Source([]byte(src))
	if err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, formatted) {
		return nil
	}
	return os.WriteFile(path, formatted, 0o644)
}

// everyGeneric has writeSources write every generic pad, for -show -all
// -generics, to see where each would land among the generics.
var everyGeneric bool

// writeSources writes the pads' files and, for every platform without
// targets, a file of constants that leaves every pad out. A generic pad is
// written only where some configuration has an anchor for it.
func writeSources(t *targets) error {
	used := map[string]bool{}
	for _, c := range t.configs {
		for _, a := range c.anchors {
			used[a.lever] = true
		}
	}
	if everyGeneric {
		for _, p := range positions {
			used["G"+p.name], used["M"+p.name] = true, true
		}
	}
	for i, p := range positions {
		if err := writeGo(filepath.Join(vmDir, p.file), positionFile(p, i == len(positions)-1, used)); err != nil {
			return err
		}
	}
	for _, o := range outers {
		if err := writeGo(filepath.Join(o.dir, "aa_layoutpad.go"), outerFile(o)); err != nil {
			return err
		}
	}
	platforms := map[string]bool{}
	for _, c := range t.configs {
		platforms["("+c.goos+" && "+c.goarch+")"] = true
		for _, f := range configFiles(c) {
			if _, err := os.Stat(f); err != nil {
				if err := writeValues(c, readValues(c)); err != nil {
					return err
				}
				break
			}
		}
	}
	keys := make([]string, 0, len(platforms))
	for k := range platforms {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("// Code generated by internal/cmd/layoutpad; DO NOT EDIT.\n\n")
	fmt.Fprintf(&b, "//go:build !(%s)\n\n", strings.Join(keys, " || "))
	b.WriteString("package vm\n\n// The layout lock has no targets for this platform: every pad is left\n// out.\nconst (\n")
	for _, l := range levers() {
		if !vmLever(l.name) {
			continue
		}
		v := -1
		if l.kind == 'd' {
			v = 0
		}
		fmt.Fprintf(&b, "\tlayoutPadN%s = %d\n", l.name, v)
	}
	b.WriteString(")\n")
	for _, a := range asmPads {
		fmt.Fprintf(&b, "\n// layoutPad%s stands for an assembly pad, which this platform has not.\nfunc layoutPad%s() {}\n", a.name, a.name)
	}
	if err := writeGo(filepath.Join(vmDir, "layoutpad_other.go"), b.String()); err != nil {
		return err
	}
	for _, o := range outers {
		b.Reset()
		b.WriteString("// Code generated by internal/cmd/layoutpad; DO NOT EDIT.\n\n")
		fmt.Fprintf(&b, "//go:build !(%s)\n\n", strings.Join(keys, " || "))
		fmt.Fprintf(&b, "package %s\n\n// The layout lock has no targets for this platform: the pad is left\n// out.\nconst layoutPadN%s = -1\n", o.pkg, o.name)
		if err := writeGo(filepath.Join(o.dir, "layoutpad_other.go"), b.String()); err != nil {
			return err
		}
	}
	return nil
}

// outerFile is an outer pad's file: the pad, the variable its stores go
// to and the init that keeps it, as package vm's last position file has
// them for vm's pads.
func outerFile(o outer) string {
	var b strings.Builder
	b.WriteString("// Code generated by internal/cmd/layoutpad; DO NOT EDIT.\n\n")
	fmt.Fprintf(&b, "package %s\n\n", o.pkg)
	fmt.Fprintf(&b, "// layoutPad%s is the layout lock's pad %s, %s.\n// It is never called: see internal/cmd/layoutpad.\n//\n//go:noinline\nfunc layoutPad%s() {\n", o.name, o.name, o.doc, o.name)
	stores(&b, "\t", "layoutPadN"+o.name)
	b.WriteString("}\n")
	fmt.Fprintf(&b, "\n// The pad's variables, never read: what its stores go to, the same size\n// whatever the lever is.\nvar (\n\tlayoutPadLive  bool\n\tlayoutPadBytes [%d]byte\n)\n", padBytes)
	fmt.Fprintf(&b, "\n// init keeps the pad in the binary: layoutPadLive is never true.\nfunc init() {\n\tif layoutPadLive && layoutPadN%s >= 0 {\n\t\tlayoutPad%s()\n\t}\n}\n", o.name, o.name)
	return b.String()
}

// stores writes a pad's body: as many byte stores as the lever's constant
// says, in groups of 1, 2, 4 ... 512 by the constant's bits, each store to
// its own byte, two apart, so that no two combine into one.
func stores(b *strings.Builder, indent, constant string) {
	for j := range storeBits {
		fmt.Fprintf(b, "%sif max(%s, 0)&(1<<%d) != 0 {\n", indent, constant, j)
		first, n := 1<<j-1, 1<<j
		for i := 0; i < n; i += storesALine {
			m := min(storesALine, n-i)
			lhs := make([]string, m)
			rhs := make([]string, m)
			for k := range m {
				lhs[k] = fmt.Sprintf("layoutPadBytes[%d]", 2*(first+i+k))
				rhs[k] = "1"
			}
			fmt.Fprintf(b, "%s\t%s = %s\n", indent, strings.Join(lhs, ", "), strings.Join(rhs, ", "))
		}
		fmt.Fprintf(b, "%s}\n", indent)
	}
}

func positionFile(p position, last bool, used map[string]bool) string {
	var b strings.Builder
	b.WriteString("// Code generated by internal/cmd/layoutpad; DO NOT EDIT.\n\n")
	if last {
		b.WriteString("//go:generate go run ../cmd/layoutpad\n\n")
	}
	b.WriteString("package vm\n\n")
	if last {
		b.WriteString("import \"unsafe\"\n\n")
	}
	g, m := used["G"+p.name], used["M"+p.name]
	if m {
		// The method comes before the function pad: laid out at the end
		// of the range before it, it moves nothing the pad holds.
		fmt.Fprintf(&b, "// layoutPadM%s calls the generic pad of the layout lock's methods at %s:\n// Go reads the bodies of methods after those of functions, so the generic\n// is laid out among those methods call first. It is never called.\n//\n//go:noinline\nfunc (r *Runtime) layoutPadM%s() {\n", p.name, p.name, p.name)
		fmt.Fprintf(&b, "\tlayoutPadM%s[struct{}]()\n}\n\n", p.name)
	}
	fmt.Fprintf(&b, "// layoutPad%s is the layout lock's pad %s, %s.\n// It is never called: see internal/cmd/layoutpad.\n//\n//go:noinline\nfunc layoutPad%s() {\n", p.name, p.name, p.doc, p.name)
	if g {
		fmt.Fprintf(&b, "\tif layoutPadNG%s >= 0 {\n\t\tlayoutPadG%s[struct{}]()\n\t}\n", p.name, p.name)
	}
	stores(&b, "\t", "layoutPadN"+p.name)
	b.WriteString("}\n")
	if g {
		fmt.Fprintf(&b, "\n// layoutPadG%s is the generic pad of layoutPad%s. It is never called.\n//\n//go:noinline\nfunc layoutPadG%s[_ any]() {\n", p.name, p.name, p.name)
		stores(&b, "\t", "layoutPadNG"+p.name)
		b.WriteString("}\n")
	}
	if m {
		fmt.Fprintf(&b, "\n// layoutPadM%s is the generic pad of (*Runtime).layoutPadM%s. It is never\n// called.\n//\n//go:noinline\nfunc layoutPadM%s[_ any]() {\n", p.name, p.name, p.name)
		stores(&b, "\t", "layoutPadNM"+p.name)
		b.WriteString("}\n")
	}
	if !last {
		return b.String()
	}
	cz := used["CZ"]
	if cz {
		b.WriteString(`
// layoutPadCZ makes the layout lock's generic closure pad, which is laid
// out first among the closures of generics. It is never called.
//
//go:noinline
func layoutPadCZ[_ any]() {
	layoutPadFunc = func() {
`)
		stores(&b, "\t\t", "layoutPadNCZ")
		b.WriteString("\t}\n}\n")
	}
	fmt.Fprintf(&b, `
// The pads' variables, never read: what the pads' stores go to, and the
// data levers, each variable 0 or its unit in size. They are the same size
// whatever the levers are but for the data levers' own.
var (
	layoutPadLive  bool
	layoutPadFunc  func()
	layoutPadBytes [%d]byte
	layoutPadPtrs  [%d]unsafe.Pointer
`, padBytes, 3*dataVars)
	for _, d := range dataLevers {
		for i := range d.n {
			fmt.Fprintf(&b, "\tlayoutPad%s%d "+d.typ+"\n", d.name, i, fmt.Sprintf("min(1, max(0, layoutPadN%s-%d))", d.name, i))
		}
	}
	b.WriteString(")\n\n")
	b.WriteString("// init keeps the pads in the binary: layoutPadLive is never true.\nfunc init() {\n\tif layoutPadLive {\n")
	for _, q := range positions {
		cond := "layoutPadN" + q.name + " >= 0"
		if used["G"+q.name] {
			cond += " || layoutPadNG" + q.name + " >= 0"
		}
		fmt.Fprintf(&b, "\t\tif %s {\n\t\t\tlayoutPad%s()\n\t\t}\n", cond, q.name)
		if used["M"+q.name] {
			fmt.Fprintf(&b, "\t\tif layoutPadNM%s >= 0 {\n\t\t\t(*Runtime)(nil).layoutPadM%s()\n\t\t}\n", q.name, q.name)
		}
	}
	if cz {
		b.WriteString("\t\tif layoutPadNCZ >= 0 {\n\t\t\tlayoutPadCZ[struct{}]()\n\t\t}\n")
	}
	for _, a := range asmPads {
		if used[a.name] {
			fmt.Fprintf(&b, "\t\tif layoutPadN%s >= 0 {\n\t\t\tlayoutPad%s()\n\t\t}\n", a.name, a.name)
		}
	}
	k := 0
	for _, d := range dataLevers {
		for i := range d.n {
			fmt.Fprintf(&b, "\t\tlayoutPadPtrs[%d] = unsafe.Pointer(&layoutPad%s%d)\n", k, d.name, i)
			k++
		}
	}
	b.WriteString("\t}\n}\n")
	return b.String()
}

// dataLevers are the data levers' variables: how many and of what type.
var dataLevers = []struct {
	name string
	n    int
	typ  string
}{{"DH", dataVars, "[%s]*byte"}, {"DW", dataVars, "[8 * %s]byte"}, {"DG", dataVars / 2, "[16 * %s]byte"}}

// solve sets c's levers so that every anchor is at its target.
func solve(c *config, verbose bool) (*symtab, error) {
	v := readValues(c)
	active := map[string]bool{}
	for _, a := range c.anchors {
		active[a.lever] = true
	}
	for _, l := range levers() {
		switch {
		case !active[l.name] && l.kind != 'd':
			v[l.name] = -1
		case !active[l.name]:
			v[l.name] = 0
		case l.kind == 'a' && v[l.name] < 1:
			// The assembly pad's least: one slot.
			v[l.name] = 1
		case l.kind != 'd' && v[l.name] < 0:
			v[l.name] = 0
		}
	}
	clear(leverBefore)
	for iter := 0; iter < 12; iter++ {
		if err := writeValues(c, v); err != nil {
			return nil, err
		}
		syms, err := build(c)
		if err != nil {
			return nil, err
		}
		if verbose {
			for _, line := range compare(c, syms) {
				fmt.Printf("  %s: %s\n", c, line)
			}
		}
		code, err := solveCode(c, syms, v, verbose)
		if err != nil {
			return nil, err
		}
		if code {
			continue
		}
		data, err := solveData(c, syms, v)
		if err != nil {
			return nil, err
		}
		if data {
			continue
		}
		fmt.Printf("%s: every anchor at its target (%s)\n", c, describe(v))
		return syms, nil
	}
	return nil, fmt.Errorf("%s: the levers did not settle", c)
}

func describe(v map[string]int) string {
	var parts []string
	for _, l := range levers() {
		if v[l.name] > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", l.name, v[l.name]))
		}
	}
	return strings.Join(parts, " ")
}

// solveCode moves each code lever's anchor to its target, in the order the
// pads are laid out, and says whether it changed a lever.
// leverBefore is each code lever's value before its last change, in the
// configuration being solved.
var leverBefore = map[string]int{}

func solveCode(c *config, syms *symtab, v map[string]int, verbose bool) (bool, error) {
	type item struct {
		l   lever
		pad symbol
		a   anchor
		at  symbol
	}
	var items []item
	for _, a := range c.anchors {
		l, _ := leverByName(a.lever)
		if l.kind == 'd' || a.fresh {
			continue
		}
		pad, ok := syms.byName[l.pad]
		if !ok {
			return false, fmt.Errorf("%s: pad %s is not in the build", c, l.pad)
		}
		at, ok := syms.byName[a.symbol]
		if !ok {
			return false, fmt.Errorf("%s: anchor %s is not in the build", c, a.symbol)
		}
		if at.addr < pad.addr {
			return false, fmt.Errorf("%s: anchor %s is before its pad %s", c, a.symbol, l.pad)
		}
		items = append(items, item{l, pad, a, at})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].pad.addr < items[j].pad.addr })
	changed := false
	var shift uint64
	for _, it := range items {
		k := v[it.l.name]
		width := roundUp(it.pad.size, slot)
		delta := (it.a.target + codeMod - (it.at.addr+shift)%codeMod) % codeMod
		if delta%slot != 0 {
			return false, fmt.Errorf("%s: anchor %s at %#x cannot reach %#x in steps of %d", c, it.a.symbol, it.at.addr, it.a.target, slot)
		}
		if delta == 0 && (it.a.at == 0 || it.at.addr+shift == it.a.at) {
			continue
		}
		if it.l.kind == 'a' {
			// The assembly pad is k whole slots of bytes: k+delta/slot of
			// them, modulo 4 KB's worth, and at least one.
			steps := uint64(codeMod / slot)
			nk := (width/slot-1+delta/slot)%steps + 1
			if exact := int64(width) + int64(it.a.at) - int64(it.at.addr+shift); it.a.at != 0 && exact >= slot {
				nk = uint64(exact) / slot
			}
			if nk*slot == width {
				continue
			}
			if verbose {
				fmt.Printf("    %s: slots %d at %#x delta %d -> %d\n", it.l.name, width/slot, it.at.addr%codeMod, delta, nk)
			}
			v[it.l.name] = int(nk)
			shift += nk*slot - width
			changed = true
			continue
		}
		// go tool nm gives a function's size with the padding after it,
		// a whole number of slots: take the code itself to end mid-slot,
		// and aim the pad's code at the middle of the slot it must end in.
		// A build that shows it a slot off moves it a store at a time.
		// In bytes, all of it integers: base is the code but the stores.
		base := int64(it.pad.size) - slot/2 - perStore*int64(k)
		minWidth := roundUp(uint64(max(base, 1)), slot)
		want := minWidth + (width+delta-minWidth)%codeMod
		// Keep the recorded page when the pad can absorb the change.
		// Wrapping a growing pad at 4 KB would move every later anchor
		// back a page even though its original address is reachable.
		if exact := int64(width) + int64(it.a.at) - int64(it.at.addr+shift); it.a.at != 0 && exact >= int64(minWidth) {
			want = uint64(exact)
		}
		if want == width {
			continue
		}
		nk := 0
		if aim := int64(want) - slot/2 - base; aim > 0 {
			nk = int((aim + perStore/2) / perStore)
		}
		switch {
		case delta == slot && nk <= k:
			nk = k + 1
		case delta == codeMod-slot && nk >= k && k > 0:
			nk = k - 1
		}
		// A pad whose stores are not all perStore bytes -- a store's
		// displacement past 127 is three bytes longer -- can overshoot
		// both ways: a lever sent back to the value it had before this
		// one is sent halfway instead, which settles.
		if p, ok := leverBefore[it.l.name]; ok && p == nk && nk != k {
			mid := (k + nk) / 2
			if mid == k {
				mid = nk
			}
			nk = mid
		}
		leverBefore[it.l.name] = k
		if nk > maxStores {
			return false, fmt.Errorf("%s: lever %s needs more than %d stores", c, it.l.name, maxStores)
		}
		if verbose {
			fmt.Printf("    %s: k %d size %d at %#x delta %d width %d want %d base %d -> %d\n", it.l.name, k, it.pad.size, it.at.addr%codeMod, delta, width, want, base, nk)
		}
		v[it.l.name] = nk
		shift += want - width
		changed = true
	}
	return changed, nil
}

func roundUp(n, m uint64) uint64 { return (n + m - 1) / m * m }

// solveData sets the first data lever whose anchor is off its target, and
// says whether it changed one. A data lever moves its anchor by the linker's
// rules for sorting and aligning data, which are cheaper to try than to
// model: the next value is tried, and solve builds again.
func solveData(c *config, syms *symtab, v map[string]int) (bool, error) {
	for _, name := range []string{"DH", "DW", "DG"} {
		var a *anchor
		for i := range c.anchors {
			if c.anchors[i].lever == name {
				a = &c.anchors[i]
			}
		}
		if a == nil {
			continue
		}
		s, ok := syms.byName[a.symbol]
		if !ok {
			return false, fmt.Errorf("%s: anchor %s is not in the build", c, a.symbol)
		}
		if s.addr%dataMod == a.target {
			continue
		}
		l, _ := leverByName(name)
		tried := dataTried[c.String()+name]
		if tried == nil {
			tried = map[int]bool{}
			dataTried[c.String()+name] = tried
		}
		tried[v[name]] = true
		for n := 0; n <= l.max; n++ {
			if !tried[n] {
				v[name] = n
				return true, nil
			}
		}
		return false, fmt.Errorf("%s: no value of %s puts %s at %#x mod %d", c, name, a.symbol, a.target, dataMod)
	}
	return false, nil
}

var dataTried = map[string]map[int]bool{}
