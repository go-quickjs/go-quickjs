package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/stdlib"
)

// TestFileURLIsPathToFileURL checks that the URL qjs gives a file --
// import.meta.url, a module's URL, the debugger's -- is what url.pathToFileURL
// gives its path, for a path with each character a name can hold, on the
// host's paths: the two are written apart, in Go and in the standard
// library's JavaScript.
func TestFileURLIsPathToFileURL(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	if err := stdlib.Install(rt, stdlib.Config{Process: &stdlib.Process{}, WindowsPaths: runtime.GOOS == "windows"}); err != nil {
		t.Fatal(err)
	}
	ns, err := rt.RequireModule("url", "")
	if err != nil {
		t.Fatal(err)
	}
	pathToFileURL, err := ns.Get("pathToFileURL")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"plain.mjs", "a b", "\u00fcn\u00ef\u20ac", "\U0001F600"}
	for c := rune(1); c < 128; c++ {
		if c != '/' && c != filepath.Separator {
			names = append(names, "a"+string(c)+"b")
		}
	}
	paths := []string{dir}
	for _, n := range names {
		paths = append(paths, filepath.Join(dir, n))
	}
	if runtime.GOOS == "windows" {
		paths = append(paths, `C:\`, `\\server\share\f`, `\\?\UNC\Server\share\a b`)
	}
	for _, p := range paths {
		u, err := pathToFileURL.Call(p)
		if err != nil {
			t.Errorf("pathToFileURL(%q): %v", p, err)
			continue
		}
		want, err := u.Get("href")
		if err != nil {
			t.Fatal(err)
		}
		if got := fileURL(p); got != want.String() {
			t.Errorf("fileURL(%q) = %s, pathToFileURL gives %s", p, got, want.String())
		}
	}
}

// TestImportMetaURLOfAnAwkwardName runs modules whose names hold characters
// a file URL escapes and some it does not, and checks that each one's
// import.meta.url is pathToFileURL of its import.meta.filename.
func TestImportMetaURLOfAnAwkwardName(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a b", "pct%41", "hash#x", "br[x]", "tick`x", "tilde~x", "excl!x", "paren(x)", "quote'x", "\u00fcn\u00ef\u20ac"} {
		p := filepath.Join(dir, n+".mjs")
		src := `import { pathToFileURL } from "node:url";
console.log(import.meta.url === pathToFileURL(import.meta.filename).href, import.meta.url.slice(import.meta.url.lastIndexOf("/") + 1));`
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		code, out, errOut := exec(t, "", p)
		if code != 0 || len(out) < 5 || out[:5] != "true " {
			t.Errorf("%s: code=%d err=%q out=%q", n, code, errOut, out)
		}
	}
}
