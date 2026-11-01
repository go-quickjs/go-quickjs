// Package bundletest describes the optional open-source bundle test corpus.
package bundletest

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Bundle pins a standalone script and an observable check of its exports.
type Bundle struct {
	Name, File, SHA256   string
	URL                  string
	Size                 int64
	Prelude, Check, Want string
}

// All are MIT-licensed upstream builds, kept outside the repository like the
// V8 benchmark suites. See docs/benchmarks.md for their URLs and licenses.
var All = []Bundle{
	{
		Name: "Three", File: "three-0.160.0.js",
		URL:     "https://unpkg.com/three@0.160.0/build/three.js",
		Size:    1315993,
		SHA256:  "7710f93ba2f67bc9886848e05a244e5414aeb116b04bfa0645053e4a3e96e6f8",
		Prelude: `var console = { warn: function() {} };`,
		Check: `JSON.stringify([THREE.REVISION, new THREE.Vector3(1, 2, 3).lengthSq(),
			new THREE.Vector3(1, 0, 0).cross(new THREE.Vector3(0, 1, 0)).toArray()])`,
		Want: `["160",14,[0,0,1]]`,
	},
	{
		Name: "Babel", File: "babel-7.26.10.min.js",
		URL:     "https://unpkg.com/@babel/standalone@7.26.10/babel.min.js",
		Size:    3014882,
		SHA256:  "7acf405141ca9e611b6d40aeef52071432d5f387cf906fc3556fb9f0b1a28601",
		Prelude: `var console = { warn: function() {}, log: function() {}, error: function() {} };`,
		Check: `JSON.stringify([Babel.version, Babel.transform("const square = x => x * x;", {
			presets: [], plugins: ["transform-arrow-functions"]
		}).code])`,
		Want: `["7.26.10","const square = function (x) {\n  return x * x;\n};"]`,
	},
}

// Read reads a pinned bundle, or skips when the optional corpus is not set.
// Once requested, missing or changed inputs fail instead of silently skipping.
func Read(tb testing.TB, bundle Bundle) string {
	tb.Helper()
	dir := os.Getenv("QUICKJS_BUNDLE_DIR")
	if dir == "" {
		tb.Skip("set QUICKJS_BUNDLE_DIR to run the open-source bundles; see docs/benchmarks.md")
	}
	b, err := os.ReadFile(filepath.Join(dir, bundle.File))
	if err != nil {
		tb.Fatal(err)
	}
	if int64(len(b)) != bundle.Size {
		tb.Fatalf("%s: size %d, want %d", bundle.File, len(b), bundle.Size)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(b)); got != bundle.SHA256 {
		tb.Fatalf("%s: SHA-256 %s, want %s", bundle.File, got, bundle.SHA256)
	}
	return string(b)
}
