package v8bench

import (
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"
)

// The suites the runners know, by the name -bench gives them. None is part
// of this repository: -fetch downloads one, at a fixed revision, into -dir.
//
//   - v8 is the V8 suite, version 7, as AreWeFastYet publishes it: the
//     figures in docs/benchmarks.md are of it.
//   - octane is Octane 2.0, the V8 suite's successor: version 7's eight
//     benchmarks, revised, and nine larger programs -- the TypeScript
//     compiler compiling itself, pdf.js, a Game Boy emulator, Box2D, zlib,
//     Mandreel, CodeLoad.
//   - awfy is Are We Fast Yet (Marr, Daloze and Mössenböck, DLS 2016), a
//     suite written to compare language implementations alike: fourteen
//     benchmarks of what object-oriented programs do -- objects, closures,
//     arrays, strings, collections of their own -- without the library
//     calls an engine's speed would be lost behind. Its JavaScript is
//     CommonJS modules, which the runners load through a require of their
//     own (awfyModules).
//   - ssr is a page React 18 renders on the server (ssr.go): React's UMD
//     builds, the page this repository's own.
type suite struct {
	name string
	// url is where the files are fetched from.
	url string
	// files are the sources the fixed mode loads, in order.
	files []string
	// extra are files fetched as well, which the fixed mode does not load:
	// run.js, which the score mode runs.
	extra []string
	// awfy marks Are We Fast Yet's harness, rather than the V8 suite's
	// BenchmarkSuite.
	awfy bool
	// pins are files fetched from URLs of their own, not url's, each read
	// only if it has its SHA-256.
	pins map[string]pin
	// ssr marks the server-rendering suite (ssr.go).
	ssr bool
}

// A pin is where a file is fetched from, and its SHA-256, in hex.
type pin struct{ url, sha256 string }

var suites = map[string]*suite{
	"v8": {
		name: "v8",
		url:  "https://raw.githubusercontent.com/mozilla/arewefastyet/0e21608/benchmarks/v8-v7/",
		files: []string{"base.js", "richards.js", "deltablue.js", "crypto.js", "raytrace.js",
			"earley-boyer.js", "regexp.js", "splay.js", "navier-stokes.js"},
		extra: []string{"run.js"},
	},
	"octane": {
		name: "octane",
		url:  "https://raw.githubusercontent.com/chromium/octane/570ad1ccfe86e3eecba0636c8f932ac08edec517/",
		files: []string{"base.js", "richards.js", "deltablue.js", "crypto.js", "raytrace.js",
			"earley-boyer.js", "regexp.js", "splay.js", "navier-stokes.js", "pdfjs.js", "mandreel.js",
			"gbemu-part1.js", "gbemu-part2.js", "code-load.js", "box2d.js", "zlib.js", "zlib-data.js",
			"typescript.js", "typescript-input.js", "typescript-compiler.js"},
		extra: []string{"run.js"},
	},
	"awfy": {
		name: "awfy",
		url:  "https://raw.githubusercontent.com/smarr/are-we-fast-yet/74306fec151070fd07157cefeacf19e7e0bcdc89/benchmarks/JavaScript/",
		files: []string{"benchmark.js", "som.js", "bounce.js", "cd.js", "deltablue.js", "havlak.js",
			"json.js", "list.js", "mandelbrot.js", "nbody.js", "permute.js", "queens.js", "richards.js",
			"sieve.js", "storage.js", "towers.js"},
		awfy: true,
	},
	"ssr": {
		name:  "ssr",
		files: []string{"react.production.min.js", "react-dom-server-legacy.browser.production.min.js"},
		pins: map[string]pin{
			"react.production.min.js": {"https://unpkg.com/react@18.3.1/umd/react.production.min.js",
				"d949f1c3687aedadcedac85261865f29b17cd273997e7f6b2bfc53b2f9d4c4dd"},
			"react-dom-server-legacy.browser.production.min.js": {"https://unpkg.com/react-dom@18.3.1/umd/react-dom-server-legacy.browser.production.min.js",
				"829b2f17da148d9bc5b635c329b67068e67e4bf7d6a5bb64e49e9e92483deee5"},
		},
		ssr: true,
	},
}

// checkPin fails a file of s's whose SHA-256 is not its pin's: one fetched
// from somewhere else, or changed since.
func checkPin(s *suite, file string, b []byte) error {
	p, ok := s.pins[file]
	if !ok {
		return nil
	}
	if sum := fmt.Sprintf("%x", sha256.Sum256(b)); sum != p.sha256 {
		return fmt.Errorf("%s: SHA-256 %s, want %s (run with -fetch to download it again)", file, sum, p.sha256)
	}
	return nil
}

// suiteNamed is the suite -bench names.
func suiteNamed(name string) *suite {
	s, ok := suites[name]
	if !ok {
		fail(fmt.Errorf("unknown suite %q: v8, octane, awfy or ssr", name))
	}
	return s
}

// awfyWork is each of Are We Fast Yet's benchmarks, by its class's name,
// its module, and the inner iterations a run makes, as its rebench.conf
// gives them to every language's harness: one run here is one of its
// measured iterations.
var awfyWork = []struct {
	name, module string
	inner        int
}{
	{"DeltaBlue", "deltablue", 12000}, {"Richards", "richards", 100}, {"Json", "json", 100},
	{"CD", "cd", 250}, {"Havlak", "havlak", 1500}, {"Bounce", "bounce", 1500},
	{"List", "list", 1500}, {"Mandelbrot", "mandelbrot", 500}, {"NBody", "nbody", 250000},
	{"Permute", "permute", 1000}, {"Queens", "queens", 1000}, {"Sieve", "sieve", 3000},
	{"Storage", "storage", 1000}, {"Towers", "towers", 600},
}

// awfyModules is Are We Fast Yet's sources as one script: each file a
// function of module, exports and require, which __awfyRequire calls once,
// the first time it is asked for, as Node does.
func awfyModules(s *suite, src map[string]string) string {
	var b strings.Builder
	b.WriteString(`var __awfyDefs = {}, __awfyCache = {};
function __awfyRequire(path) {
	var name = path.replace(/^\.\//, "").replace(/\.js$/, "");
	var m = __awfyCache[name];
	if (m) return m.exports;
	m = __awfyCache[name] = { exports: {} };
	__awfyDefs[name](m, m.exports, __awfyRequire);
	return m.exports;
}
`)
	for _, f := range s.files {
		fmt.Fprintf(&b, "__awfyDefs[%s] = function (module, exports, require) {\n%s\n};\n",
			strconv.Quote(strings.TrimSuffix(f, ".js")), src[f])
	}
	return b.String()
}

// awfyRun is the script one benchmark's n runs are, each a check of its
// result: what Are We Fast Yet's harness measures, without its clock.
func awfyRun(module string, inner, n int) string {
	return fmt.Sprintf(`(() => {
	const b = __awfyRequire(%q).newInstance();
	for (let i = 0; i < %d; i++) {
		if (!b.innerBenchmarkLoop(%d)) throw new Error(%q + ": the result is wrong");
	}
	return "";
})()`, "./"+module, n, inner, module)
}
