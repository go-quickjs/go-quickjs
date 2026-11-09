package wasm

import (
	"github.com/go-quickjs/go-quickjs/wasm/internal/engine"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/names"
)

// compiledCode is a decoded, validated module and its engine code, shared
// by every CompiledModule of the same bytes in runtimes with one cache.
type compiledCode struct {
	// m is the module as the engine runs it: its start function is called
	// by the runtime after instantiation, not by the engine.
	m        *module.Module
	code     *engine.Code
	start    uint32
	hasStart bool
}

// names returns the module's names, read on first use.
func (c *compiledCode) names() *names.Names { return names.Of(c.m) }
