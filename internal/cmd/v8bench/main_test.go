package main

import (
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/internal/v8bench"
)

func TestJITBenchmarkRequiresNativeBuild(t *testing.T) {
	if jit.Supported() {
		t.Skip("this build includes native execution")
	}
	previous := *jitFlag
	*jitFlag = true
	defer func() { *jitFlag = previous }()
	_, err := (engine{}).NewRuntime(v8bench.Host{})
	if err == nil || !strings.Contains(err.Error(), "-tags quickjs_jit") {
		t.Fatalf("native benchmark silently selected fallback: %v", err)
	}
}
