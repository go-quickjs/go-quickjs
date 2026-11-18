package stdlib_test

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/stdlib"
)

// TestURLMatchesNode runs the url corpus -- url.pathToFileURL and
// url.fileURLToPath in both flavors over relative paths, drives, UNC shares,
// every character a file URL escapes, encoded separators and hosts, and
// their errors -- and compares every answer with node's, which
// testdata/urlcorpus_node.js recorded in testdata/url_node.json.
func TestURLMatchesNode(t *testing.T) {
	data, err := os.ReadFile("testdata/url_node.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]string
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile("testdata/urlcorpus.js")
	if err != nil {
		t.Fatal(err)
	}

	rt := quickjs.New()
	defer rt.Close()
	if err := stdlib.Install(rt, stdlib.Config{Process: &stdlib.Process{}}); err != nil {
		t.Fatal(err)
	}
	corpus, err := rt.EvalFile("urlcorpus.js", string(src))
	if err != nil {
		t.Fatal(err)
	}
	ns, err := rt.RequireModule("url", "")
	if err != nil {
		t.Fatal(err)
	}
	url, err := ns.Get("default")
	if err != nil {
		t.Fatal(err)
	}
	setCwd, err := rt.Eval(`(cwd) => { process.cwd = () => cwd }`)
	if err != nil {
		t.Fatal(err)
	}
	answers, err := corpus.Call(url, setCwd)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := answers.Decode(&got); err != nil {
		t.Fatal(err)
	}

	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var diffs []string
	for _, k := range keys {
		if got[k] != want[k] {
			diffs = append(diffs, k+"\n    got  "+got[k]+"\n    node "+want[k])
		}
	}
	if len(got) != len(want) {
		t.Errorf("%d answers, node gave %d", len(got), len(want))
	}
	if len(diffs) > 0 {
		shown := diffs[:min(len(diffs), 40)]
		t.Errorf("%d of %d answers differ from node's:\n%s", len(diffs), len(want), strings.Join(shown, "\n"))
	}
}

// TestURLFlavorFollowsWindowsPaths checks which flavor url's functions take
// when no options say: path.win32's where the runtime's paths are Windows'
// (Config.WindowsPaths), as node's do on Windows, and path.posix's
// otherwise; and that a root's trailing separator is checked against the
// runtime's own, as node checks it against the host's: on Windows the posix
// root "/" ends in no backslash, so a slash is added to it, and elsewhere
// the Windows root C:\ ends in no slash.
func TestURLFlavorFollowsWindowsPaths(t *testing.T) {
	for _, c := range []struct {
		windows   bool
		cwd, file string
		want      string
	}{
		{false, `/w`, `file:///x/y`, `file:///w/x file:/// file:///C:// /x/y`},
		{true, `C:\\w`, `file:///C:/x/y`, `file:///C:/w/x file://// file:///C:/ C:\x\y`},
	} {
		rt := quickjs.New()
		if err := stdlib.Install(rt, stdlib.Config{Process: &stdlib.Process{}, WindowsPaths: c.windows}); err != nil {
			t.Fatal(err)
		}
		_, err := rt.EvalModule("flavor.mjs", `import { pathToFileURL, fileURLToPath } from "node:url";
process.cwd = () => "`+c.cwd+`";
globalThis.answer = [pathToFileURL("x").href, pathToFileURL("/", { windows: false }).href,
  pathToFileURL("C:\\", { windows: true }).href, fileURLToPath("`+c.file+`")].join(" ");`)
		var got quickjs.Value
		if err == nil {
			got, err = rt.Eval(`answer`)
		}
		if err != nil {
			rt.Close()
			t.Fatal(err)
		}
		if got.String() != c.want {
			t.Errorf("WindowsPaths %v: %s, want %s", c.windows, got.String(), c.want)
		}
		rt.Close()
	}
}

// TestURLPathsAsTheStandardSays pins what the URL parser makes of a path,
// each as node's URL makes it: empty segments kept, %2e as a dot, a file
// URL's drive kept from a double-dot segment and from a reference from the
// root, a drive written C| and a reference starting with one, the host
// localhost of a file URL, which is none, an empty host of another scheme
// kept, and an invalid URL's error.
func TestURLPathsAsTheStandardSays(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	if err := stdlib.Install(rt, stdlib.Config{}); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][3]string{
		{"file:////", "", "file:////"},
		{"http://x/a//b", "", "http://x/a//b"},
		{"http://x/a/./b//../c", "", "http://x/a/b/c"},
		{"http://x/a/b/..//c", "", "http://x/a//c"},
		{"http://x/a/%2e/b", "", "http://x/a/b"},
		{"http://x/a/%2E%2e/b", "", "http://x/b"},
		{"file:///C:/a/../../b", "", "file:///C:/b"},
		{"http://x/a/b//..", "", "http://x/a/b/"},
		{"file://localhost/a", "", "file:///a"},
		{"file://LOCALHOST/b", "", "file:///b"},
		{"file:///C|/a", "", "file:///C:/a"},
		{"foo:///a/../b", "", "foo:///b"},
		{"foo://h", "", "foo://h"},
		{"c:/../x", "", "c:/x"},
		{"mailto:a/../b", "", "mailto:a/../b"},
		{"/e", "file:///C:/a/b", "file:///C:/e"},
		{"C|/k", "file:///C:/a/b", "file:///C:/k"},
		{"../../../d", "file:///C:/a/b", "file:///C:/d"},
	} {
		f, err := rt.Eval(`(u, b) => b === "" ? new URL(u).href : new URL(u, b).href`)
		if err != nil {
			t.Fatal(err)
		}
		got, err := f.Call(c[0], c[1])
		if err != nil {
			t.Errorf("new URL(%q, %q): %v", c[0], c[1], err)
			continue
		}
		if got.String() != c[2] {
			t.Errorf("new URL(%q, %q) = %s, want %s", c[0], c[1], got.String(), c[2])
		}
	}
	got, err := rt.Eval(`try { new URL("x") } catch (e) { [e.name, e.code, e.message, e.input].join(" | ") }`)
	if err != nil {
		t.Fatal(err)
	}
	if want := "TypeError | ERR_INVALID_URL | Invalid URL | x"; got.String() != want {
		t.Errorf("an invalid URL threw %s, want %s", got.String(), want)
	}
}
