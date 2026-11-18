package engine

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The register tests' expected answers (testdata/reg.golden): each call's
// results, or its error and the offset of its trap, by test and call. They
// were recorded from the closure tree, the interpreter before register
// code, so that register code is held to what the two agreed on;
// WASM_REG_GOLDEN=update records them again from the Go table, which a
// change to what a test calls needs.

var regGolden struct {
	once sync.Once
	m    map[string]string
	mu   sync.Mutex
	new  map[string]string
}

const regGoldenFile = "testdata/reg.golden"

func regGoldenUpdate() bool { return os.Getenv("WASM_REG_GOLDEN") == "update" }

func loadRegGolden(t *testing.T) {
	regGolden.once.Do(func() {
		regGolden.m = map[string]string{}
		regGolden.new = map[string]string{}
		f, err := os.Open(regGoldenFile)
		if err != nil {
			if !regGoldenUpdate() && os.Getenv("WASM_REG_GOLDEN") != "tree" {
				t.Fatal(err)
			}
			return
		}
		defer f.Close()
		s := bufio.NewScanner(f)
		s.Buffer(nil, 1<<20)
		for s.Scan() {
			k, v, ok := strings.Cut(s.Text(), " => ")
			if ok {
				regGolden.m[k] = v
			}
		}
	})
}

// regAnswer is a call's answer as the golden file writes it.
func regAnswer(got []Val, err error) string {
	if err != nil {
		var tr *Trap
		if errors.As(err, &tr) && len(tr.Frames) > 0 {
			return fmt.Sprintf("error %q at %#x", err.Error(), tr.Frames[0].Offset)
		}
		return fmt.Sprintf("error %q", err.Error())
	}
	parts := make([]string, len(got))
	for i, v := range got {
		switch {
		case v.R.IsNull() && v.Hi == 0:
			parts[i] = fmt.Sprintf("%#x", v.N)
		case v.R.IsNull():
			parts[i] = fmt.Sprintf("%#x:%#x", v.N, v.Hi)
		case v.R.IsI31():
			parts[i] = fmt.Sprintf("i31 %#x", v.R.I31Unsigned())
		case v.R.Kind() == ObjHost:
			parts[i] = fmt.Sprintf("host %v", (*Host)(v.R.p).V)
		default:
			parts[i] = fmt.Sprintf("ref kind %d", v.R.Kind())
		}
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// regKey is call c's key in the golden file: the test's name and the
// call's.
func regKey(t *testing.T, c regCall) string {
	args := make([]string, len(c.args))
	for i, a := range c.args {
		args[i] = regAnswer([]Val{a}, nil)
	}
	return fmt.Sprintf("%s %s(%s)", t.Name(), c.name, strings.Join(args, ", "))
}

// regMatchesGolden runs calls on the module of src compiled to register
// code, natively where the native handlers are built and on the Go table,
// and fails where an answer differs from the golden file's.
func regMatchesGolden(t *testing.T, src string, calls []regCall) {
	t.Helper()
	loadRegGolden(t)
	ways := []bool{false}
	if nativeBuilt {
		ways = append(ways, true)
	}
	if os.Getenv("WASM_REG_GOLDEN") == "tree" {
		inst := instantiateMode(t, src, false)
		for _, c := range calls {
			ext, _ := inst.Export(c.name)
			got, err := Invoke(context.Background(), ext.Func, c.args)
			regGolden.mu.Lock()
			regGolden.new[regKey(t, c)] = regAnswer(got, err)
			regGolden.mu.Unlock()
		}
		return
	}
	for _, native := range ways {
		inst := instantiateReg(t, src)
		for _, c := range calls {
			ext, ok := inst.Export(c.name)
			if !ok {
				t.Fatalf("no export %q", c.name)
			}
			var got []Val
			var err error
			withNative(native, func() { got, err = Invoke(context.Background(), ext.Func, c.args) })
			if regOf(inst, c.name) == nil {
				t.Errorf("%s runs no register code", c.name)
			}
			key, answer := regKey(t, c), regAnswer(got, err)
			if regGoldenUpdate() {
				if !native {
					regGolden.mu.Lock()
					regGolden.new[key] = answer
					regGolden.mu.Unlock()
				}
				continue
			}
			want, ok := regGolden.m[key]
			switch {
			case !ok:
				t.Errorf("%s: no golden answer (WASM_REG_GOLDEN=update records it)", key)
			case answer != want:
				t.Errorf("%s (native %v) = %s, the golden %s", key, native, answer, want)
			}
		}
	}
}

// writeRegGolden writes the answers an update recorded, the file's own
// for the tests that did not run kept.
func writeRegGolden() error {
	if !regGoldenUpdate() && os.Getenv("WASM_REG_GOLDEN") != "tree" || len(regGolden.new) == 0 {
		return nil
	}
	for k, v := range regGolden.new {
		regGolden.m[k] = v
	}
	keys := make([]string, 0, len(regGolden.m))
	for k := range regGolden.m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	var b strings.Builder
	b.WriteString("# The register tests' expected answers (reg_golden_test.go).\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s => %s\n", k, regGolden.m[k])
	}
	if err := os.MkdirAll(filepath.Dir(regGoldenFile), 0o755); err != nil {
		return err
	}
	return os.WriteFile(regGoldenFile, []byte(b.String()), 0o644)
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func TestMain(m *testing.M) {
	code := m.Run()
	if err := writeRegGolden(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}
