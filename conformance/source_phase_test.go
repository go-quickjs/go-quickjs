package conformance_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/conformance"
	"github.com/go-quickjs/go-quickjs/internal/hostaccess"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

type testModuleSource struct{}

func (testModuleSource) ModuleSourceClassName() string { return "Test262.ModuleSource" }

// The suite asks for a host module with a source object, independent of any
// particular module format. Every referrer resolves it to the same module.
func installModuleSource(rt *quickjs.Runtime) error {
	r := hostaccess.VM(rt)
	proto, err := rt.AbstractModuleSource().Get("prototype")
	if err != nil {
		return err
	}
	source := vm.Obj(vm.HostObject(hostaccess.Unwrap(proto).Object(), testModuleSource{}))
	vm.HostSetModuleCompiler(r, func(name, _ string) (*vm.Module, bool, error) {
		if name != "<module source>" {
			return nil, false, nil
		}
		return vm.HostDefineModule(r, vm.HostModule{
			Name: name, Source: source,
			Evaluate: func([]*vm.Object) ([]vm.Value, error) { return nil, nil },
		}), true, nil
	})
	return nil
}

func TestSourcePhaseImportSyntax(t *testing.T) {
	const feature = "source-phase-imports-module-source"
	const syntax = "language/expressions/dynamic-import/syntax/"
	for _, tc := range []struct {
		name, path, source, extraFeature string
		negative                         *conformance.Negative
		want                             result
	}{
		{name: "valid", path: syntax + "valid/source.js", source: `import.source("<module source>");`, want: resultPass},
		{name: "invalid", path: syntax + "invalid/source.js", source: `import.source(...[]);`,
			negative: &conformance.Negative{Phase: "parse", Type: "SyntaxError"}, want: resultPass},
		{name: "syntax failure is reported", path: syntax + "valid/source.js", source: `import.source(...[]);`, want: resultFail},
		{name: "runtime failure is reported", path: syntax + "valid/source.js", source: `throw new Error("failure");`, want: resultFail},
		{name: "other unsupported feature", path: syntax + "valid/source.js", source: `0;`, extraFeature: "decorators", want: resultSkip},
		{name: "source object test failure", path: "staging/source-phase-imports/source.js", source: `throw new Error("failure");`, want: resultFail},
		{name: "module source test failure", path: "language/module-code/source-phase-import/source.js", source: `throw new Error("failure");`, want: resultFail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			features := []string{feature}
			if tc.extraFeature != "" {
				features = append(features, tc.extraFeature)
			}
			test := &conformance.Test{
				Path: tc.path, Source: tc.source,
				Meta: conformance.Metadata{
					Features: features, Flags: map[string]bool{"raw": true}, Negative: tc.negative,
				},
			}
			var stats vm.JITStats
			got, reason := runOne(&conformance.Suite{}, test, nil, &stats)
			if got != tc.want {
				t.Fatalf("result = %v (%s), want %v", got, reason, tc.want)
			}
			if got == resultSkip {
				want := unsupportedFeatures[tc.extraFeature]
				if reason != want {
					t.Errorf("skip reason = %q, want %q", reason, want)
				}
			}
		})
	}
}

func TestModuleSourceFixture(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "test"), 0755); err != nil {
		t.Fatal(err)
	}
	for name, source := range map[string]string{
		"left.js":  `import source x from '<module source>'; export { x };`,
		"right.js": `import source x from '<module source>'; export { x };`,
		"both.js":  `export * from './left.js'; export * from './right.js';`,
		"plain.js": `export const x = 1;`,
	} {
		if err := os.WriteFile(filepath.Join(root, "test", name), []byte(source), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, source string
		module       bool
	}{
		{"static and reexported", `
			import source direct from '<module source>';
			import { x } from './both.js';
			import * as ns from './left.js';
			if (!(direct instanceof $262.AbstractModuleSource)) throw new Error('source prototype');
			if (direct !== x || direct !== ns.x) throw new Error('source identity');
			if (Object.prototype.toString.call(direct) !== '[object Test262.ModuleSource]')
				throw new Error('source tag');
		`, true},
		{"dynamic", `
			Promise.all([import.source('<module source>'), import.source('<module source>')])
				.then(([a, b]) => {
					if (!(a instanceof $262.AbstractModuleSource) || a !== b)
					throw new Error('dynamic source identity');
					return import.source('./plain.js').then(
						() => { throw new Error('JavaScript has no module source'); },
						e => { if (!(e instanceof SyntaxError)) throw e; });
				}).then(() => print('Test262:AsyncTestComplete'), e => { throw e; });
		`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stats vm.JITStats
			got, reason := runOne(&conformance.Suite{Root: root}, &conformance.Test{
				Path: "main.js", Source: tc.source,
				Meta: conformance.Metadata{
					Features: []string{"source-phase-imports-module-source"},
					Flags:    map[string]bool{"raw": true, "module": tc.module, "async": !tc.module},
				},
			}, nil, &stats)
			if got != resultPass {
				t.Fatalf("result = %v (%s), want pass", got, reason)
			}
		})
	}
}
