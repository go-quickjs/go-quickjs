package regexp

import (
	"bytes"
	"os"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/regexp/internal/asciigen"
)

// TestASCIIMatcherGenerated pins that exec_ascii.go is what go generate makes
// of exec.go: the matcher over bytes kept the same as the one over code units.
func TestASCIIMatcherGenerated(t *testing.T) {
	want, err := asciigen.Generate("exec.go")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("exec_ascii.go")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")), want) {
		t.Fatal("exec_ascii.go is out of date: run go generate ./internal/regexp")
	}
}
