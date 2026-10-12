package quickjs_test

import (
	"os"
	"strings"
	"testing"
)

// TestNoGojaDependency pins that go-quickjs does not depend on goja: the
// benchmark runner that compares the two is a module of its own, and a
// dependency of this one would be in its go.mod or its go.sum.
func TestNoGojaDependency(t *testing.T) {
	for _, f := range []string{"go.mod", "go.sum"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "goja") {
			t.Errorf("%s mentions goja", f)
		}
	}
	if _, err := os.Stat("internal/cmd/v8bench/goja/go.mod"); err != nil {
		t.Errorf("the goja benchmark runner is not a module of its own: %v", err)
	}
}

// TestNoDisassemblerDependency pins the same for golang.org/x/arch, whose
// disassemblers check the JIT's encodings from internal/jit/verify, a module
// of its own.
func TestNoDisassemblerDependency(t *testing.T) {
	for _, f := range []string{"go.mod", "go.sum"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "golang.org/x/arch") {
			t.Errorf("%s mentions golang.org/x/arch", f)
		}
	}
	if _, err := os.Stat("internal/jit/verify/go.mod"); err != nil {
		t.Errorf("the encoder verifier is not a module of its own: %v", err)
	}
}
