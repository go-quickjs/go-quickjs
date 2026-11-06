package vm

import (
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// A random program of appends, and of everything that could see a string
// grow: reads, aliases, conversions that run code, generators, throws. Its
// value is a log of what it saw, every string it kept hashed again at the
// end, so that a string changed after it was kept shows.
//
// The reference is the program compiled for a debugger, which has no append
// of the compiler's (ownedAppend, localAppend) and no str_freeze: strings
// there are what they always were. It takes no memory limit, as the JIT's
// differential tests do: a meter turns appends and arrays' room off, and these
// are what is being tested.
type strFuzz struct {
	rng  *rand.Rand
	vars []string
	// sloppy says the function may use with and eval.
	sloppy bool
	serial int
}

const strFuzzPrelude = `
const log = [], keep = [], thunks = []
function hash(s) {
  if (typeof s !== 'string') return String(s)
  let h = 7, step = 1 + (s.length >> 8)
  for (let i = 0; i < s.length; i += step) h = (h * 31 + s.charCodeAt(i)) | 0
  for (let i = Math.max(0, s.length - 8); i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) | 0
  return s.length + ':' + h
}
const hi = String.fromCharCode(0xd83d), lo = String.fromCharCode(0xde00), wide = String.fromCharCode(0xe9), cjk = String.fromCharCode(0x65e5)
const long = 'x'.repeat(70), longw = cjk.repeat(30)
function esc(s) { return '<' + s + '>' }
function pass(v) { return v }
function note(v) { if (log.length < 4000) log.push(hash(v)); return v }
function kept(v) { if (keep.length < 400) keep.push(v); else keep[log.length % 400] = v; return v }
function again(n, f) { for (let i = 0; i < n; i++) f(i) }
`

func (g *strFuzz) pick(list ...string) string { return list[g.rng.IntN(len(list))] }

func (g *strFuzz) v() string { return g.vars[g.rng.IntN(len(g.vars))] }

// text is a string expression that runs no code of the program's.
func (g *strFuzz) text() string {
	return g.pick(`'a'`, `'bc'`, `'<td>'`, `'</td>'`, `'chunk'`, `''`, `hi`, `lo`, `hi+lo`, `wide`, `cjk`, `long`, `longw`,
		`'é'`, `'x'.repeat(9)`, `'y'.repeat(100)`, `String(i)`)
}

// operand is what may stand on an append's right.
func (g *strFuzz) operand() string {
	v := g.v()
	switch g.rng.IntN(14) {
	case 0:
		return "esc(" + g.text() + ")"
	case 1:
		return "i"
	case 2:
		return g.pick("null", "undefined", "true", "1.5", "2n")
	case 3:
		return "`${" + g.text() + "}`"
	case 4:
		return "(i % 2 ? " + g.text() + " : " + g.text() + ")"
	case 5:
		return "pass(" + g.text() + ")"
	case 6:
		return "(" + g.text() + " + " + g.text() + ")"
	case 7:
		return "conv" + v
	case 8:
		return "val" + v
	case 9:
		return "top"
	}
	return g.text()
}

// bounded is stmt, unless v is long: what an append of a string to itself
// or another could double without end.
func bounded(v, stmt string) string {
	return "if (typeof " + v + " !== 'string' || " + v + ".length < 40000) { " + stmt + " }"
}

func (g *strFuzz) read() string {
	v, w := g.v(), g.v()
	return g.pick(
		"note("+v+")", "kept("+v+")", "note("+v+".length)", "note("+v+".slice?.(2, 9))", "note(String("+v+").charCodeAt(i))",
		"saved = "+v, "o.k = "+v, "arr[i & 3] = "+v, "m.set(i & 3, "+v+")", "kept(o.k)", "kept(arr[1])", "kept(m.get(2))", "kept(saved)",
		"thunks.push(() => "+v+")", "kept((function () { return arguments[0] })("+v+"))",
		"kept((function (...a) { return a[0] })("+v+"))", "note([...String("+v+")].length)", "kept([...["+v+"]][0])",
		"kept(((p = "+v+") => p)())", "try { throw "+v+" } catch (e) { kept(e) }",
		"kept(`${"+v+"}`)", "note(String("+v+").concat('z'))", "kept(String("+v+"))", "kept("+v+" + '')", "kept('' + "+v+")",
		"note(typeof "+v+")", "note("+v+" === "+w+")", "note("+v+" < "+w+")", "note("+v+" == "+w+")",
		"note(({ ["+v+"]: 1 }, 1))", "o2[String("+v+")] = i", "switch ("+v+") { case "+w+": log.push('same'); break; default: log.push('other') }",
		"note(JSON.stringify("+v+"))", "note(String("+v+").at(-1))", "note(String("+v+").indexOf('x'))", "note(/x/.test("+v+"))",
		"note(String("+v+").replace('a', 'b'))", "note(String("+v+").split('x').length)", "note(String("+v+").trimEnd())",
		"kept("+v+"), kept("+v+")", "kept(o.g"+v+")", "note("+v+" + "+w+")", "note(["+v+", "+w+"].join())", "kept(new String("+v+") + '')",
		"for (const c of String("+v+").slice(0, 3)) note(c)", "({ a: saved } = { a: "+v+" })", "[saved] = ["+v+"]",
		"note(String("+v+").substring(1, 12))", "kept(String("+v+").substring(0, 66))", "kept(String("+v+").slice(0, -1))",
	)
}

func (g *strFuzz) write() string {
	v, w := g.v(), g.v()
	return g.pick(
		v+" = "+g.text(), v+" = "+w, v+" = saved", v+" = ''", v+" = 5", v+" ||= "+g.text(), v+" ??= "+g.text(),
		bounded(v, v+" &&= "+v+" + "+g.text()), v+" = String("+v+")", v+"++", v+" = pass("+v+")",
	)
}

func (g *strFuzz) grow() string {
	v, w := g.v(), g.v()
	x := g.operand()
	return g.pick(
		v+" += "+x, v+" += "+x, v+" += "+x, v+" += "+x, v+" += "+x, v+" += "+x,
		"add"+v+"("+x+")", "add"+v+"("+x+")", "add"+v+"("+x+")", "add"+v+"("+x+")",
		"(i & 1) !== 0 && ("+v+" += "+x+")", "(i & 1) !== 0 || ("+v+" += "+x+")", "sink"+v+".push("+x+")",
		bounded(v, v+" += "+v), bounded(v, "add"+v+"("+v+")"), bounded(v, "dbl"+v+"()"), bounded(v, v+" = "+v+" + "+v),
		bounded(v, v+" += "+w), bounded(v, "add"+v+"("+w+")"), bounded(v, v+" = "+w+" + "+v),
		bounded(v, v+" += pass("+v+")"), bounded(v, v+" += "+v+".length"),
		v+" = "+v+" + "+x, v+" = "+x+" + "+v,
	)
}

// stmt is one statement, in a loop whose counter is i.
func (g *strFuzz) stmt(depth int) string {
	k := g.rng.IntN(32)
	if depth >= 3 && k >= 22 {
		k %= 22
	}
	switch {
	case k < 11:
		return g.grow()
	case k < 18:
		return g.read()
	case k < 21:
		return g.write()
	case k == 21:
		return g.pick("if (i === 3) continue", "if (i === 9) break")
	case k < 24:
		return "if (i % " + strconv.Itoa(2+g.rng.IntN(3)) + " === 0) { " + g.block(depth+1) + " } else { " + g.block(depth+1) + " }"
	case k == 24:
		return "try { " + g.block(depth+1) + " } finally { " + g.block(depth+1) + " }"
	case k == 25:
		return "try { " + g.block(depth+1) + "; if (i % 4 === 1) throw 1; " + g.block(depth+1) + " } catch { " + g.block(depth+1) + " }"
	case k == 26:
		return "for (let j = 0; j < " + strconv.Itoa(1+g.rng.IntN(6)) + "; j++) { " + g.block(depth+1) + " }"
	case k == 27:
		return "{ let i2 = i; again(2, () => { " + g.closureBlock() + " }) }"
	case k == 28:
		return "for (const y of gen(i)) { " + g.block(depth+1) + "; log.push(y) }"
	case k == 29:
		if g.sloppy {
			return g.pick("with (o) { "+g.block(depth+1)+" }", "kept(eval('"+g.v()+"'))", "eval('"+g.v()+" += \"e\"')")
		}
	case k == 30:
		return "for (const k in { a: 1, b: 2 }) { " + g.block(depth+1) + " }"
	}
	return g.grow()
}

func (g *strFuzz) block(depth int) string {
	var b strings.Builder
	for range 1 + g.rng.IntN(3) {
		b.WriteString(g.stmt(depth))
		b.WriteString("; ")
	}
	return b.String()
}

// closureBlock is what runs in a closure of the function: no break.
func (g *strFuzz) closureBlock() string {
	var b strings.Builder
	for range 1 + g.rng.IntN(3) {
		if g.rng.IntN(2) == 0 {
			b.WriteString(g.grow())
		} else {
			b.WriteString(g.read())
		}
		b.WriteString("; ")
	}
	return b.String()
}

// fn writes a function of the program.
func (g *strFuzz) fn(b *strings.Builder, name string) {
	n := 1 + g.rng.IntN(3)
	g.vars = g.vars[:0]
	for i := range n {
		g.vars = append(g.vars, fmt.Sprintf("s%d", i))
	}
	g.sloppy = g.rng.IntN(6) == 0
	star := ""
	if g.rng.IntN(5) == 0 {
		star = "*"
	}
	fmt.Fprintf(b, "function%s %s(top) {\n", star, name)
	// A reader may come first, which the compiler then sees before the appends.
	reader := func(v string) {
		fmt.Fprintf(b, "  function rd%s() { return %s }\n", v, v)
	}
	if g.rng.IntN(5) == 0 {
		reader(g.v())
	}
	for _, v := range g.vars {
		decl := g.pick("var", "var", "let")
		init := g.pick(`''`, `''`, `'<ul>'`, `long`, `longw`, `top`, `'' + top`, `hi`, `lo + long`, `0`, `undefined`)
		if init == "undefined" {
			fmt.Fprintf(b, "  %s %s;\n", decl, v)
		} else {
			fmt.Fprintf(b, "  %s %s = %s;\n", decl, v, init)
		}
	}
	fmt.Fprintln(b, "  var saved = '', o = {}, o2 = {}, arr = [], m = new Map()")
	for _, v := range g.vars {
		// Each use of these may be left out of a function: a closure that
		// is never made reads nothing, and leaves a binding a local.
		if g.rng.IntN(4) != 0 {
			fmt.Fprintf(b, "  const add%s = x => { %s += x }\n", v, v)
		} else {
			fmt.Fprintf(b, "  const add%s = x => { %s = %s + x }\n", v, v, v)
		}
		fmt.Fprintf(b, "  const dbl%s = () => { %s += %s }\n", v, v, v)
		fmt.Fprintf(b, "  const sink%s = { push(x) { x !== null && (%s += x); return true } }\n", v, v)
		fmt.Fprintf(b, "  const conv%s = { toString() { %s; return 'Y' } }\n", v, g.pick(v+" += 'Q'", "add"+v+"('Q')", "kept("+v+")", v+" = 'reset'", "note(1)"))
		fmt.Fprintf(b, "  const val%s = { valueOf() { %s; return 7 } }\n", v, g.pick(v+" += 'V'", "add"+v+"('V')", "kept("+v+")"))
		fmt.Fprintf(b, "  Object.defineProperty(o, 'g%s', { get() { return %s } })\n", v, v)
	}
	if g.rng.IntN(3) == 0 {
		reader(g.v())
	}
	fmt.Fprintln(b, "  function* gen(i) { var y = 0; while (y < 2) { "+g.closureBlock()+" yield y++ } }")
	if g.rng.IntN(3) == 0 {
		v := g.v()
		fmt.Fprintf(b, "  o = { get peek() { return %s }, toString() { return %s } }\n", v, v)
	}
	loops := []int{1, 3, 8, 40}[g.rng.IntN(4)]
	fmt.Fprintf(b, "  for (let i = 0; i < %d; i++) {\n", loops)
	for range 1 + g.rng.IntN(6) {
		fmt.Fprint(b, "    ", g.stmt(0), ";\n")
	}
	fmt.Fprintln(b, "  }")
	for _, v := range g.vars {
		fmt.Fprintf(b, "  note(%s)\n", v)
	}
	if star != "" {
		fmt.Fprintf(b, "  yield %s\n", g.vars[0])
	}
	fmt.Fprintf(b, "  return %s\n}\n", g.vars[0])
}

// localFn writes a function of the form localAppend takes: nothing closes
// over its string.
func (g *strFuzz) localFn(b *strings.Builder, name string) {
	star := ""
	if g.rng.IntN(4) == 0 {
		star = "*"
	}
	fmt.Fprintf(b, "function%s %s(top) {\n  var out = %s, other = ''\n", star, name, g.pick(`''`, `'<td>'`, `long`, `'' + top`, `hi`))
	fmt.Fprintln(b, "  var saved = ''")
	loops := []int{2, 5, 40}[g.rng.IntN(3)]
	fmt.Fprintf(b, "  for (let i = 0; i < %d; i++) {\n", loops)
	for range 2 + g.rng.IntN(7) {
		x := g.text()
		fmt.Fprintf(b, "    %s;\n", g.pick(
			"out += "+x, "out += "+x, "out += "+x, "out += esc("+x+")", "out += esc(pass("+x+"))", "out += pass(esc("+x+"))",
			"out += `<${"+x+"}>`", "out += i", "out += null", "out += {toString() { return "+x+" }}",
			"(i & 1) !== 0 && (out += "+x+")", "try { out += esc("+x+"); if (i === 3) throw 1 } catch { out += 'c' }",
			"try { out += (() => { throw 2 })() } catch { note(out.length) }", "note(out.length)", "note(out)", "kept(out)",
			"saved = out", "kept(saved)", "other = out + 'q'", "other += out", "if (i % 5 === 4) out = other", "if (i % 7 === 6) out = ''",
			func() string {
				if star != "" {
					return "out += yield out.length"
				}
				return "out += 'y'"
			}(),
			func() string {
				if star != "" {
					return "out += yield"
				}
				return "out += 'z'"
			}(),
			"for (const k in { a: 1, b: 2 }) out += k",
			"while (out.length < 20) out += 'w'",
		))
	}
	fmt.Fprintln(b, "  }")
	if star != "" {
		fmt.Fprintln(b, "  yield out")
	}
	fmt.Fprintln(b, "  return out\n}")
}

// source is a program: its functions, run in rounds so that each tier meets
// them, with every function's generators driven.
func (g *strFuzz) source() string {
	var b strings.Builder
	b.WriteString(strFuzzPrelude)
	count := 1 + g.rng.IntN(3)
	for i := range count {
		name := fmt.Sprintf("f%d", i)
		if g.rng.IntN(3) == 0 {
			g.localFn(&b, name)
		} else {
			g.fn(&b, name)
		}
	}
	rounds := 1 + g.rng.IntN(5)
	if g.rng.IntN(3) == 0 {
		// Past the calls that compile a function and what it calls, with what
		// each round left to read.
		rounds = 24
	}
	fmt.Fprintf(&b, "for (let round = 0; round < %d; round++) {\n", rounds)
	for i := range count {
		fmt.Fprintf(&b, "  { let r = f%d(%s); if (r && r.next) { let sent = 'sent'; for (let k = 0, s = r.next(); !s.done && k < 200; k++, s = r.next(sent + k)) note(s.value); } else note(r) }\n",
			i, g.pick(`'t'`, `long`, `'t' + round`, `round`, `hi`))
	}
	b.WriteString("  for (const t of thunks) note(t())\n  thunks.length = 0\n}\n")
	b.WriteString("log.concat(keep.map(hash)).join()\n")
	return b.String()
}

// strFuzzSeed is where a run's programs start, and strFuzzCount how many
// there are: a soak sets QJS_STRFUZZ, "seed,count".
func strFuzzRange(t *testing.T, count int) (int, int) {
	first := 1
	if spec := os.Getenv("QJS_STRFUZZ"); spec != "" {
		s, c, ok := strings.Cut(spec, ",")
		var err, err2 error
		first, err = strconv.Atoi(s)
		if ok {
			count, err2 = strconv.Atoi(c)
		}
		if err != nil || err2 != nil {
			t.Fatal("QJS_STRFUZZ is seed,count")
		}
	}
	return first, count
}

func strFuzzSource(seed int) string {
	g := &strFuzz{rng: rand.New(rand.NewPCG(uint64(seed), 0x5eed))}
	return g.source()
}

// strFuzzRun is a program's value, an uncaught exception's message, or what
// says it took too long.
func strFuzzRun(t *testing.T, r *Runtime, src string, debug bool) string {
	t.Helper()
	prog, err := parser.Parse(src, parser.Options{})
	if err != nil {
		t.Fatalf("%v\n%s", err, src)
	}
	fn, err := compiler.Compile(prog, compiler.Options{Source: "test", Text: src, Debug: debug})
	if err != nil {
		t.Fatalf("%v\n%s", err, src)
	}
	got, _ := func() (string, time.Duration) {
		v, err := r.Run(fn)
		if err != nil {
			return "uncaught: " + err.Error(), 0
		}
		s, err := r.ToString(v)
		if err != nil {
			return "uncaught: " + err.Error(), 0
		}
		return s.Go(), 0
	}()
	return got
}

func TestStrAppendRandom(t *testing.T) {
	t.Cleanup(func() { SetTreeTier(true) })
	count := 400
	if testing.Short() {
		count = 40
	}
	first, count := strFuzzRange(t, count)
	for seed := first; seed < first+count; seed++ {
		src := strFuzzSource(seed)
		ref := New(Config{Debug: true})
		want := strFuzzRun(t, ref, src, true)
		ref.Close()
		for _, tree := range []bool{false, true} {
			SetTreeTier(tree)
			r := New(Config{})
			got := strFuzzRun(t, r, src, false)
			r.Close()
			if got != want {
				t.Fatalf("seed %d, tree tier %v: %s\n%s", seed, tree, firstLogDifference(got, want), src)
			}
		}
	}
}

// firstLogDifference is where two logs part.
func firstLogDifference(got, want string) string {
	g, w := strings.Split(got, ","), strings.Split(want, ",")
	for i := range min(len(g), len(w)) {
		if g[i] != w[i] {
			return fmt.Sprintf("entry %d of %d: got %s, want %s", i, len(w), g[i], w[i])
		}
	}
	return fmt.Sprintf("%d entries, want %d", len(g), len(w))
}
