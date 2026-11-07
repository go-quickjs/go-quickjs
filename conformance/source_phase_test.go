package conformance_test

import (
	"testing"

	"github.com/go-quickjs/go-quickjs/conformance"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// The host-source feature tag must not hide grammar failures or bypass the
// feature checks for tests that really need a module source object.
func TestSourcePhaseImportSyntax(t *testing.T) {
	const feature = "source-phase-imports-module-source"
	const syntax = "language/expressions/dynamic-import/syntax/"
	for _, tc := range []struct {
		name, path, source, extraFeature string
		negative                         *conformance.Negative
		force                            bool
		want                             result
	}{
		{name: "valid", path: syntax + "valid/source.js", source: `import.source("<module source>");`, want: resultPass},
		{name: "invalid", path: syntax + "invalid/source.js", source: `import.source(...[]);`,
			negative: &conformance.Negative{Phase: "parse", Type: "SyntaxError"}, want: resultPass},
		{name: "syntax failure is reported", path: syntax + "valid/source.js", source: `import.source(...[]);`, want: resultFail},
		{name: "runtime failure is reported", path: syntax + "valid/source.js", source: `throw new Error("failure");`, want: resultFail},
		{name: "other unsupported feature", path: syntax + "valid/source.js", source: `0;`, extraFeature: "decorators", want: resultSkip},
		{name: "source object required", path: "staging/source-phase-imports/source.js", source: `throw new Error("must be skipped");`, want: resultSkip},
		{name: "module source binding", path: "language/module-code/source-phase-import/source.js", source: `throw new Error("must be skipped");`, want: resultSkip},
		{name: "force host feature", path: "staging/source-phase-imports/source.js", source: `throw new Error("forced");`, force: true, want: resultFail},
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
			forced := map[string]bool{feature: tc.force}
			var stats vm.JITStats
			got, reason := runOne(&conformance.Suite{}, test, forced, &stats)
			if got != tc.want {
				t.Fatalf("result = %v (%s), want %v", got, reason, tc.want)
			}
			if got == resultSkip {
				want := unsupportedFeatures[feature]
				if tc.extraFeature != "" {
					want = unsupportedFeatures[tc.extraFeature]
				}
				if reason != want {
					t.Errorf("skip reason = %q, want %q", reason, want)
				}
			}
		})
	}
}
