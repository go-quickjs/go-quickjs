package wasm

import (
	"context"

	"github.com/go-quickjs/go-quickjs/wasm/api"
	"github.com/go-quickjs/go-quickjs/wasm/internal/internalapi"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// CompiledModule is a module decoded and validated, ready to be instantiated
// any number of times. It may be used from several goroutines at once.
//
// Closing the Runtime closes every CompiledModule it compiled.
type CompiledModule interface {
	// Name returns the name in the module's name section, or the host
	// module's name; it may be empty.
	Name() string

	// ImportedFunctions returns the imported functions, in import order.
	ImportedFunctions() []api.FunctionDefinition

	// ExportedFunctions returns the exported functions, keyed by export
	// name.
	ExportedFunctions() map[string]api.FunctionDefinition

	// ImportedMemories returns the imported memories, in import order.
	ImportedMemories() []api.MemoryDefinition

	// ExportedMemories returns the exported memories, keyed by export name.
	ExportedMemories() map[string]api.MemoryDefinition

	// CustomSections returns the module's custom sections, in order, if
	// the runtime keeps them (WithCustomSections), and nil otherwise.
	CustomSections() []api.CustomSection

	// Close releases the compiled module. Modules instantiated from it stay
	// usable.
	Close(context.Context) error
}

// compiledModule is a CompiledModule: a wasm module's code, or a host
// module's functions.
type compiledModule struct {
	r      *runtime
	code   *compiledCode // nil for a host module
	host   *hostModule   // nil for a wasm module
	custom bool
}

func (c *compiledModule) Name() string {
	if c.host != nil {
		return c.host.name
	}
	return c.code.names().Module
}

func (c *compiledModule) ImportedFunctions() []api.FunctionDefinition {
	if c.host != nil {
		return nil
	}
	m := c.code.m
	exports := exportNamesOf(m, module.ExternFunc)
	var out []api.FunctionDefinition
	for _, imp := range m.Imports {
		if imp.Kind == module.ExternFunc {
			out = append(out, c.code.funcDefinition(imp.Index, exports))
		}
	}
	return out
}

func (c *compiledModule) ExportedFunctions() map[string]api.FunctionDefinition {
	out := map[string]api.FunctionDefinition{}
	if c.host != nil {
		for _, f := range c.host.funcs {
			for _, name := range f.def.exportNames {
				out[name] = f.def
			}
		}
		return out
	}
	m := c.code.m
	exports := exportNamesOf(m, module.ExternFunc)
	for _, e := range m.Exports {
		if e.Kind == module.ExternFunc {
			out[e.Name] = c.code.funcDefinition(e.Index, exports)
		}
	}
	return out
}

func (c *compiledModule) ImportedMemories() []api.MemoryDefinition {
	if c.host != nil {
		return nil
	}
	m := c.code.m
	exports := exportNamesOf(m, module.ExternMemory)
	var out []api.MemoryDefinition
	for _, imp := range m.Imports {
		if imp.Kind == module.ExternMemory {
			out = append(out, c.code.memoryDefinition(imp.Index, exports))
		}
	}
	return out
}

func (c *compiledModule) ExportedMemories() map[string]api.MemoryDefinition {
	out := map[string]api.MemoryDefinition{}
	if c.host != nil {
		return out
	}
	m := c.code.m
	exports := exportNamesOf(m, module.ExternMemory)
	for _, e := range m.Exports {
		if e.Kind == module.ExternMemory {
			out[e.Name] = c.code.memoryDefinition(e.Index, exports)
		}
	}
	return out
}

func (c *compiledModule) CustomSections() []api.CustomSection {
	if c.host != nil || !c.custom {
		return nil
	}
	m := c.code.m
	out := make([]api.CustomSection, len(m.Customs))
	for i, s := range m.Customs {
		out[i] = &customSection{name: s.Name, data: m.Bytes[s.Data.Offset:s.Data.End():s.Data.End()]}
	}
	return out
}

func (c *compiledModule) Close(context.Context) error { return nil }

type customSection struct {
	internalapi.WasmOnlyType
	name string
	data []byte
}

func (s *customSection) Name() string { return s.name }
func (s *customSection) Data() []byte { return s.data }
