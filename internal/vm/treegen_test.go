package vm

import (
	"bytes"
	"os"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/vm/internal/treegen"
)

// TestTreeOperandsGenerated fails while tree_operand.go is not what its
// generator writes.
func TestTreeOperandsGenerated(t *testing.T) {
	want, err := treegen.Generate()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile("tree_operand.go")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")), want) {
		t.Error("tree_operand.go is out of date: run go generate ./internal/vm")
	}
}
