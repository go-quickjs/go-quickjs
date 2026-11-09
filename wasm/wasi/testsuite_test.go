package wasi_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm"
	"github.com/go-quickjs/go-quickjs/wasm/sys"
	"github.com/go-quickjs/go-quickjs/wasm/wasi"
)

// testsuiteSpec is a test's JSON specification in wasi-testsuite's legacy
// form.
type testsuiteSpec struct {
	Args       []string          `json:"args"`
	Env        map[string]string `json:"env"`
	Root       *string           `json:"root"`
	ExitCode   uint32            `json:"exit_code"`
	Stdout     *string           `json:"stdout"`
	Stderr     *string           `json:"stderr"`
	Operations []json.RawMessage `json:"operations"`
}

// TestWASITestsuite runs the prebuilt wasm32-wasip1 tests of
// github.com/WebAssembly/wasi-testsuite (its prod/testsuite-base branch),
// named by WASI_TESTSUITE_DIR:
//
//	git clone -b prod/testsuite-base https://github.com/WebAssembly/wasi-testsuite
//	WASI_TESTSUITE_DIR=$PWD/wasi-testsuite/tests go test ./wasm/wasi -run TestWASITestsuite -v
func TestWASITestsuite(t *testing.T) {
	root := os.Getenv("WASI_TESTSUITE_DIR")
	if root == "" {
		t.Skip("WASI_TESTSUITE_DIR is not set")
	}
	var tests []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && strings.HasSuffix(d.Name(), ".dir") {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(p, ".wasm") && filepath.Base(filepath.Dir(p)) == "wasm32-wasip1" {
			tests = append(tests, p)
		}
		return nil
	})
	sort.Strings(tests)
	if len(tests) == 0 {
		t.Fatalf("no wasm32-wasip1 tests under %s", root)
	}
	ctx := context.Background()
	r := wasm.NewRuntime(ctx)
	defer r.Close(ctx)
	wasi.MustInstantiate(ctx, r)
	passed, skipped := 0, 0
	for _, path := range tests {
		rel, _ := filepath.Rel(root, path)
		ok := t.Run(filepath.ToSlash(rel), func(t *testing.T) {
			var spec testsuiteSpec
			if b, err := os.ReadFile(strings.TrimSuffix(path, ".wasm") + ".json"); err == nil {
				if err := json.Unmarshal(b, &spec); err != nil {
					t.Fatal(err)
				}
			}
			if spec.Operations != nil {
				skipped++
				t.Skip("an operation-based specification")
			}
			bin, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			cfg := wasm.NewModuleConfig().WithName("").
				WithArgs(append([]string{filepath.Base(path)}, spec.Args...)...).
				WithStdout(&stdout).WithStderr(&stderr).
				WithSysWalltime().WithSysNanotime().WithSysNanosleep().
				WithRandSource(rand.Reader)
			keys := make([]string, 0, len(spec.Env))
			for k := range spec.Env {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				cfg = cfg.WithEnv(k, spec.Env[k])
			}
			if spec.Root != nil {
				dir := t.TempDir()
				if err := os.CopyFS(dir, os.DirFS(filepath.Join(filepath.Dir(path), *spec.Root))); err != nil {
					t.Fatal(err)
				}
				cfg = cfg.WithFSConfig(wasm.NewFSConfig().WithDirMount(dir, "/"))
			}
			mod, err := r.InstantiateWithConfig(ctx, bin, cfg)
			if mod != nil {
				mod.Close(ctx)
			}
			code := uint32(0)
			var exit *sys.ExitError
			if errors.As(err, &exit) {
				code = exit.ExitCode()
			} else if err != nil {
				t.Fatalf("%v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
			}
			if code != spec.ExitCode {
				t.Errorf("exit code %d, want %d\nstdout:\n%s\nstderr:\n%s", code, spec.ExitCode, stdout.String(), stderr.String())
			}
			if spec.Stdout != nil && stdout.String() != *spec.Stdout {
				t.Errorf("stdout %q, want %q", stdout.String(), *spec.Stdout)
			}
			if spec.Stderr != nil && stderr.String() != *spec.Stderr {
				t.Errorf("stderr %q, want %q", stderr.String(), *spec.Stderr)
			}
		})
		if ok {
			passed++
		}
	}
	t.Logf("wasi-testsuite: %d of %d passed, %d skipped", passed-skipped, len(tests), skipped)
}
