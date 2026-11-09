package wasm

import (
	"math"

	"github.com/go-quickjs/go-quickjs/wasm/api"
	"github.com/go-quickjs/go-quickjs/wasm/internal/internalapi"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

type exportDefinition struct {
	internalapi.WasmOnlyType
	moduleName            string
	index                 uint32
	importModule, import_ string
	isImport              bool
	exportNames           []string
}

func (d *exportDefinition) ModuleName() string    { return d.moduleName }
func (d *exportDefinition) Index() uint32         { return d.index }
func (d *exportDefinition) ExportNames() []string { return d.exportNames }
func (d *exportDefinition) Import() (string, string, bool) {
	return d.importModule, d.import_, d.isImport
}

type functionDefinition struct {
	exportDefinition
	name                    string
	goFunc                  interface{}
	params, results         []api.ValueType
	paramNames, resultNames []string
}

func (d *functionDefinition) Name() string                 { return d.name }
func (d *functionDefinition) GoFunction() interface{}      { return d.goFunc }
func (d *functionDefinition) ParamTypes() []api.ValueType  { return d.params }
func (d *functionDefinition) ParamNames() []string         { return d.paramNames }
func (d *functionDefinition) ResultTypes() []api.ValueType { return d.results }
func (d *functionDefinition) ResultNames() []string        { return d.resultNames }

func (d *functionDefinition) DebugName() string {
	if d.name != "" {
		return d.moduleName + "." + d.name
	}
	return d.moduleName + ".$" + uitoa(d.index)
}

type memoryDefinition struct {
	exportDefinition
	limits module.Limits
	shared bool
}

func sat32(n uint64) uint32 {
	if n > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(n)
}

func (d *memoryDefinition) Min() uint32           { return sat32(d.limits.Initial) }
func (d *memoryDefinition) Max() (uint32, bool)   { return sat32(d.limits.Max), d.limits.HasMax }
func (d *memoryDefinition) Min64() uint64         { return d.limits.Initial }
func (d *memoryDefinition) Max64() (uint64, bool) { return d.limits.Max, d.limits.HasMax }
func (d *memoryDefinition) Is64() bool            { return d.limits.Is64 }
func (d *memoryDefinition) IsShared() bool        { return d.shared }

func uitoa(n uint32) string {
	if n == 0 {
		return "0"
	}
	var b [10]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// exportNames returns the names each entity of kind k is exported as.
func exportNamesOf(m *module.Module, k module.ExternKind) map[uint32][]string {
	out := map[uint32][]string{}
	for _, e := range m.Exports {
		if e.Kind == k {
			out[e.Index] = append(out[e.Index], e.Name)
		}
	}
	return out
}

// importOf returns the import of entity index of kind k, if it is one.
func importOf(m *module.Module, k module.ExternKind, index uint32) (module.Import, bool) {
	for _, imp := range m.Imports {
		if imp.Kind == k && imp.Index == index {
			return imp, true
		}
	}
	return module.Import{}, false
}

// funcDefinition returns the definition of function index of the module c.
func (c *compiledCode) funcDefinition(index uint32, exports map[uint32][]string) *functionDefinition {
	m := c.m
	n := c.names()
	ft := m.FuncType(index)
	d := &functionDefinition{
		exportDefinition: exportDefinition{moduleName: n.modName, index: index, exportNames: exports[index]},
		name:             n.funcs[index],
		params:           typeCodes(ft.Params, m),
		results:          typeCodes(ft.Results, m),
	}
	if imp, ok := importOf(m, module.ExternFunc, index); ok {
		d.importModule, d.import_, d.isImport = imp.Module, imp.Name, true
	}
	if locals := n.locals[index]; len(ft.Params) > 0 && locals != nil {
		names := make([]string, len(ft.Params))
		for i := range names {
			s, ok := locals[uint32(i)]
			if !ok {
				names = nil
				break
			}
			names[i] = s
		}
		d.paramNames = names
	}
	return d
}

// memoryDefinition returns the definition of memory index of the module c.
func (c *compiledCode) memoryDefinition(index uint32, exports map[uint32][]string) *memoryDefinition {
	m := c.m
	mem := m.Memories[index]
	d := &memoryDefinition{
		exportDefinition: exportDefinition{moduleName: c.names().modName, index: index, exportNames: exports[index]},
		limits:           mem.Limits,
		shared:           mem.Shared,
	}
	if imp, ok := importOf(m, module.ExternMemory, index); ok {
		d.importModule, d.import_, d.isImport = imp.Module, imp.Name, true
	}
	return d
}
