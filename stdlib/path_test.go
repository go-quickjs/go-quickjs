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

// TestPathMatchesNode runs the path corpus -- path.posix and path.win32 over
// paths with roots, drives, UNC shares, devices, dots and separators of both
// kinds, and their errors -- and compares every answer with node's, which
// testdata/pathcorpus_node.js recorded in testdata/path_node.json.
func TestPathMatchesNode(t *testing.T) {
	data, err := os.ReadFile("testdata/path_node.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]string
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile("testdata/pathcorpus.js")
	if err != nil {
		t.Fatal(err)
	}

	rt := quickjs.New()
	defer rt.Close()
	if err := stdlib.Install(rt, stdlib.Config{Process: &stdlib.Process{}}); err != nil {
		t.Fatal(err)
	}
	corpus, err := rt.EvalFile("pathcorpus.js", string(src))
	if err != nil {
		t.Fatal(err)
	}
	ns, err := rt.RequireModule("path", "")
	if err != nil {
		t.Fatal(err)
	}
	path, err := ns.Get("default")
	if err != nil {
		t.Fatal(err)
	}
	setCwd, err := rt.Eval(`(cwd) => { process.cwd = () => cwd }`)
	if err != nil {
		t.Fatal(err)
	}
	answers, err := corpus.Call(path, setCwd)
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
