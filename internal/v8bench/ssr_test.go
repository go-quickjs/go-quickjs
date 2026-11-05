package v8bench

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// A fetched file is read only if it has its pin's SHA-256: one changed
// since, or fetched from somewhere else, fails with both sums.
func TestSSRPins(t *testing.T) {
	s := &suite{name: "pinned", pins: map[string]pin{"a.js": {"https://example.invalid/a.js",
		"ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb"}}}
	if err := checkPin(s, "a.js", []byte("a")); err != nil {
		t.Fatalf("the pinned bytes: %v", err)
	}
	err := checkPin(s, "a.js", []byte("b"))
	if err == nil || !strings.Contains(err.Error(), "want ca978112") {
		t.Fatalf("other bytes: %v", err)
	}
	if err := checkPin(s, "other.js", []byte("b")); err != nil {
		t.Fatalf("a file with no pin: %v", err)
	}
}

// A run checks each render's length: a page of any other length fails it,
// saying how long it was.
func TestSSRRunChecksThePage(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.Eval(`globalThis.__ssrRender = (n) => "x".repeat(globalThis.length)`); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Eval(`globalThis.length = ` + strconv.Itoa(ssrWant)); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Eval(ssrRun(3)); err != nil {
		t.Fatalf("the page's length: %v", err)
	}
	if _, err := rt.Eval(`globalThis.length = 5`); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Eval(ssrRun(1)); err == nil || !strings.Contains(err.Error(), "the page is 5 characters long") {
		t.Fatalf("a page of 5 characters: %v", err)
	}
}

// ssrFakeReact is just enough of React for the page to run: elements as
// objects, function components called, and markup with no escaping.
const ssrFakeReact = `
globalThis.React = { createElement: (type, props, ...children) => ({ type, props: props || {}, children }) };
globalThis.ReactDOMServer = { renderToString: function render(e) {
	if (e === null || e === undefined || e === false) return "";
	if (Array.isArray(e)) return e.map(render).join("");
	if (typeof e !== "object") return String(e);
	if (typeof e.type === "function") return render(e.type(Object.assign({}, e.props, { children: e.children })));
	return "<" + e.type + ">" + e.children.map(render).join("") + "</" + e.type + ">";
} };`

// The page runs: every product's components, made of the data, render.
func TestSSRPage(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	for _, src := range []string{ssrPrelude, ssrFakeReact, ssrPage} {
		if _, err := rt.EvalFile("ssr.js", src); err != nil {
			t.Fatal(err)
		}
	}
	v, err := rt.Eval(`[__ssrRender(1), __ssrRender(` + strconv.Itoa(ssrProducts) + `).split("<article>").length - 1]`)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String(); !strings.HasPrefix(got, "<html><head><title>Shop</title></head><body><nav>") || !strings.HasSuffix(got, ","+strconv.Itoa(ssrProducts)) {
		t.Fatalf("the page: %.200s", got)
	}
}

// With React's builds fetched into V8BENCH_SSR_DIR (v8bench -bench ssr
// -fetch -dir), the page is the length the runs check, in every tier.
func TestSSRReact(t *testing.T) {
	dir := os.Getenv("V8BENCH_SSR_DIR")
	if dir == "" {
		t.Skip("set V8BENCH_SSR_DIR to a directory v8bench -bench ssr -fetch filled")
	}
	s := suiteNamed("ssr")
	src := map[string]string{}
	for _, f := range s.files {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if err := checkPin(s, f, b); err != nil {
			t.Fatal(err)
		}
		src[f] = string(b)
	}
	for _, opts := range [][]quickjs.Option{nil, {quickjs.WithJIT()}} {
		rt := quickjs.New(opts...)
		for _, f := range ssrScripts(s, src) {
			if _, err := rt.EvalFile(f[0], f[1]); err != nil {
				t.Fatalf("%s: %v", f[0], err)
			}
		}
		if _, err := rt.Eval(ssrRun(3)); err != nil {
			t.Fatal(err)
		}
		rt.Close()
	}
}

// The fresh suite's scripts answer as its benchmarks check, and a request's
// data has the same length whichever the request.
func TestFreshScripts(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	if _, err := rt.Eval(`globalThis.__ssrRenderData = d => JSON.stringify(d.products.map(p => p.reviews.length))`); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Eval(freshKept); err != nil {
		t.Fatal(err)
	}
	if a, b := freshData(1), freshData(999); len(a) != len(b) {
		t.Errorf("requests' scripts are %d and %d bytes", len(a), len(b))
	}
	v, err := rt.Eval(freshData(7))
	if err != nil {
		t.Fatal(err)
	}
	if want := strconv.Itoa(2*freshProducts + 1); v.String() != want {
		t.Errorf("the data's script answered %q, want %q", v.String(), want)
	}
	if got, err := rt.Eval(freshSmall); err != nil || got.String() != freshSmallWant {
		t.Errorf("the small script: %v, %v, want %s", got, err, freshSmallWant)
	}
	check := freshCheck("t", "")
	if err := check("10"); err != nil {
		t.Error(err)
	}
	if err := check("11"); err == nil {
		t.Error("a different answer passed")
	}
}
