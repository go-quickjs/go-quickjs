package opt

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm/internal/decoder"
	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/wast"
)

// TestBuildSpec builds every defined function of every module of the spec
// scripts as SSA, verifying the IR as built and after each pass, and
// counts the functions this tier refuses, by reason.
func TestBuildSpec(t *testing.T) {
	dir := os.Getenv("WASM_SPEC_DIR")
	if dir == "" {
		t.Skip("WASM_SPEC_DIR is not set")
	}
	var files []string
	for _, sub := range []string{"core", "legacy"} {
		filepath.WalkDir(filepath.Join(dir, "test", sub), func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".wast") {
				files = append(files, p)
			}
			return nil
		})
	}
	sort.Strings(files)
	built, values := 0, 0
	refused := map[string]int{}
	failures := 0
	for _, file := range files {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		cmds, err := wast.Parse(src)
		if err != nil {
			continue
		}
		for _, cmd := range cmds {
			c, ok := cmd.(*wast.ModuleCommand)
			if !ok {
				continue
			}
			m := decode(c.Module)
			if m == nil {
				continue
			}
			for fn := range m.Functions {
				if m.Functions[fn].Imported {
					continue
				}
				f, err := Build(m, uint32(fn))
				if err != nil {
					var r *wjit.Refusal
					if !errors.As(err, &r) {
						t.Fatalf("%s line %d: function %d: %v", filepath.Base(file), c.Line(), fn, err)
					}
					refused[r.Reason]++
					continue
				}
				fail := func(pass string, err error) {
					failures++
					if failures <= 20 {
						t.Errorf("%s line %d: function %d after %s: %v\n%v", filepath.Base(file), c.Line(), fn, pass, err, f)
					}
				}
				if err := Verify(f); err != nil {
					fail("build", err)
					continue
				}
				ok := true
				Optimize(f, func(pass string) {
					if ok {
						if err := Verify(f); err != nil {
							fail(pass, err)
							ok = false
						}
					}
				})
				if ok {
					if err := checkLowerable(f); err != nil {
						fail("optimize", err)
					}
				}
				built++
				values += f.NumValues()
			}
		}
	}
	total := 0
	for _, n := range refused {
		total += n
	}
	t.Logf("built %d functions (%d values), refused %d: %v", built, values, total, refused)
}

func decode(m *wast.Module) *module.Module {
	b, err := m.Binary()
	if err != nil {
		return nil
	}
	lim := decoder.CoreLimits()
	mod, derr := decoder.Decode(b, lim)
	if derr != nil {
		return nil
	}
	if derr := decoder.ValidateFunctions(mod, lim); derr != nil {
		return nil
	}
	return mod
}
