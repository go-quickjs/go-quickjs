//go:build !quickjs_jit || android || ios || !((linux && amd64) || (windows && amd64) || (darwin && arm64))

package jit

import (
	"errors"
	"testing"
)

func TestBackendExcluded(t *testing.T) {
	if Supported() {
		t.Fatal("unsupported build reports native support")
	}
	code, err := NewArena().place([]byte{0})
	if code != nil || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("place = %v, %v; want nil, ErrUnavailable", code, err)
	}
}
