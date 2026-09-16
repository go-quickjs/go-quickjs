package conformance_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/conformance"
)

// The test262 conformance run.
//
// The suite is large -- tens of thousands of files, most of which run twice --
// so it is not part of the ordinary `go test ./...`. Point it at a checkout:
//
//	TEST262_DIR=/path/to/test262 go test ./conformance -run TestConformance
//
// Add -v to see the first failures in each area, and -conformance.report to
// write the full list of failing paths for triage.

var (
	reportPath = flag.String("conformance.report", "",
		"write the list of failing tests to this file")
	subdirFlag = flag.String("conformance.dir", "",
		"restrict the run to a comma-separated list of directories under test/")
	maxFailures = flag.Int("conformance.max-failures", 40,
		"how many failures to print before summarizing")
	testTimeout = flag.Duration("conformance.timeout", 5*time.Second,
		"how long any one test may run before being counted as a timeout")
)

// result is the outcome of one test.
type result uint8

const (
	resultPass result = iota
	resultFail
	resultSkip
)

// unsupportedFeatures lists the test262 feature tags for things this engine
// does not implement. A test tagged with one is skipped rather than counted as
// a failure, because it is testing something that was never claimed.
var unsupportedFeatures = map[string]string{
	"Intl.Locale":                   "no internationalization API",
	"Temporal":                      "no Temporal",
	"Atomics":                       "no shared memory",
	"SharedArrayBuffer":             "no shared memory",
	"resizable-arraybuffer":         "no resizable buffers",
	"decorators":                    "decorators are not implemented",
	"import-assertions":             "no module attributes",
	"import-attributes":             "no module attributes",
	"explicit-resource-management":  "no using declarations",
	"Array.fromAsync":               "no async iteration helpers",
	"iterator-helpers":              "no iterator helpers",
	"source-phase-imports":          "no source phase imports",
	"import-defer":                  "no deferred imports",
	"tail-call-optimization":        "no tail calls",
	"Intl-enumeration":              "no internationalization API",
	"FinalizationRegistry":          "no finalization registry",
	"WeakRef":                       "no weak references",
	"ShadowRealm":                   "no shadow realms",
	"regexp-duplicate-named-groups": "no duplicate named groups",
	"regexp-modifiers":              "no inline regexp modifiers",
	"uint8array-base64":             "no base64 helpers",
	"Math.sumPrecise":               "no Math.sumPrecise",
	"promise-try":                   "no Promise.try",
	"Error.isError":                 "no Error.isError",
	"RegExp.escape":                 "no RegExp.escape",
	"set-methods":                   "no Set methods",
	"json-parse-with-source":        "no JSON source access",
	"Intl.DurationFormat":           "no internationalization API",
	"legacy-regexp":                 "no legacy RegExp statics",
	"String.prototype.replaceAll":   "",
	"IsHTMLDDA":                     "no document.all emulation",
	"cross-realm":                   "no realms API",
	"Reflect.construct":             "",
	"caller":                        "no legacy caller access",
	"arguments-object":              "",
}

func TestConformance(t *testing.T) {
	suite, err := conformance.Open("")
	if err != nil {
		t.Fatalf("opening the suite: %v", err)
	}
	if suite == nil {
		t.Skip("no test262 checkout found; set TEST262_DIR to run the conformance suite")
	}

	var subdirs []string
	if *subdirFlag != "" {
		subdirs = strings.Split(*subdirFlag, ",")
	} else {
		// language/ and built-ins/ are the two areas this engine targets. The
		// rest of the suite covers host integration and annexes it does not
		// claim.
		subdirs = []string{"language", "built-ins"}
	}

	tests, err := suite.Load(subdirs)
	if err != nil {
		t.Fatalf("loading tests: %v", err)
	}
	t.Logf("loaded %d test variants from %s", len(tests), suite.Root)

	var (
		pass, fail, skip int
		failures         []string
		byArea           = map[string]*areaStats{}
	)

	for _, tc := range tests {
		res, reason := runOne(suite, tc)
		area := areaOf(tc.Path)
		st := byArea[area]
		if st == nil {
			st = &areaStats{}
			byArea[area] = st
		}

		switch res {
		case resultPass:
			pass++
			st.pass++
		case resultSkip:
			skip++
			st.skip++
		default:
			fail++
			st.fail++
			failures = append(failures, tc.Name()+": "+reason)
			if len(failures) <= *maxFailures {
				t.Logf("FAIL %s: %s", tc.Name(), reason)
			}
		}
	}

	total := pass + fail
	rate := 0.0
	if total > 0 {
		rate = 100 * float64(pass) / float64(total)
	}
	t.Logf("test262: %d passed, %d failed, %d skipped (%.2f%% of executed)",
		pass, fail, skip, rate)

	// A per-area breakdown makes it obvious where the remaining work is.
	areas := make([]string, 0, len(byArea))
	for a := range byArea {
		areas = append(areas, a)
	}
	sort.Slice(areas, func(i, j int) bool {
		return byArea[areas[i]].fail > byArea[areas[j]].fail
	})
	t.Log("failures by area:")
	for _, a := range areas {
		st := byArea[a]
		if st.fail == 0 {
			continue
		}
		t.Logf("  %-44s %5d failed / %5d run", a, st.fail, st.pass+st.fail)
	}

	if *reportPath != "" {
		sort.Strings(failures)
		if err := os.WriteFile(*reportPath, []byte(strings.Join(failures, "\n")+"\n"), 0o644); err != nil {
			t.Errorf("writing the report: %v", err)
		}
		t.Logf("wrote %d failures to %s", len(failures), *reportPath)
	}
}

type areaStats struct{ pass, fail, skip int }

// areaOf groups a test path into a reportable area, which is the first two
// path segments.
func areaOf(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) > 2 {
		return strings.Join(parts[:2], "/")
	}
	return parts[0]
}

// runOne executes a single test and classifies the outcome.
func runOne(suite *conformance.Suite, tc *conformance.Test) (result, string) {
	for _, f := range tc.Meta.Features {
		if reason, unsupported := unsupportedFeatures[f]; unsupported && reason != "" {
			return resultSkip, reason
		}
	}
	// Modules are not implemented, so a module test is skipped rather than
	// counted against the engine.
	if tc.Meta.Flags["module"] {
		return resultSkip, "modules are not implemented"
	}
	// CanBlockIsFalse and similar host hooks are not provided.
	if tc.Meta.Flags["CanBlockIsFalse"] || tc.Meta.Flags["CanBlockIsTrue"] {
		return resultSkip, "no agent support"
	}

	prelude, err := suite.Prelude(tc)
	if err != nil {
		return resultSkip, err.Error()
	}

	rt := quickjs.New(quickjs.WithMaxCallDepth(400))
	defer rt.Close()

	// The suite's async tests report completion through print.
	var printed []string
	rt.Set("print", func(s string) { printed = append(printed, s) })

	// Several tests import a fixture file sitting beside them, so the loader
	// resolves relative to the test's own directory.
	dir := path.Dir(tc.Path)
	rt.SetModuleLoader(func(specifier, referrer string) (string, string, error) {
		base := dir
		if referrer != "" {
			base = path.Dir(referrer)
		}
		resolved := path.Join(base, specifier)
		b, err := os.ReadFile(filepath.Join(suite.Root, "test", filepath.FromSlash(resolved)))
		if err != nil {
			return "", "", err
		}
		return string(b), resolved, nil
	})

	// Some tests loop for a very long time, and a few loop forever. The
	// context bounds every one of them, which is the same mechanism a host
	// would use and exercises it thoroughly as a side effect.
	ctx, cancel := context.WithTimeout(context.Background(), *testTimeout)
	defer cancel()

	if prelude != "" {
		if _, err := rt.EvalContext(ctx, prelude); err != nil {
			return resultFail, "harness failed: " + summarize(err)
		}
	}

	_, runErr := rt.EvalContext(ctx, tc.Body())
	if runErr != nil && errors.Is(runErr, context.DeadlineExceeded) {
		return resultFail, "timed out"
	}

	if neg := tc.Meta.Negative; neg != nil {
		if runErr == nil {
			return resultFail, fmt.Sprintf("expected a %s %s but the test passed", neg.Phase, neg.Type)
		}
		if !matchesNegative(runErr, neg) {
			return resultFail, fmt.Sprintf("expected %s, got: %s", neg.Type, summarize(runErr))
		}
		return resultPass, ""
	}

	if runErr != nil {
		return resultFail, summarize(runErr)
	}

	// An async test has not passed until it prints the completion marker.
	if tc.Meta.Flags["async"] {
		for _, line := range printed {
			if line == "Test262:AsyncTestComplete" {
				return resultPass, ""
			}
			if strings.HasPrefix(line, "Test262:AsyncTestFailure") {
				return resultFail, line
			}
		}
		return resultFail, "the async test did not complete"
	}
	return resultPass, ""
}

// matchesNegative reports whether an error is the failure the test expected.
func matchesNegative(err error, neg *Negative) bool {
	var syntaxErr *quickjs.SyntaxError
	isParseError := errors.As(err, &syntaxErr)

	if neg.Phase == "parse" || neg.Phase == "resolution" {
		// A test expecting a parse error has not passed if the engine accepted
		// the source and failed later.
		if !isParseError {
			return false
		}
		return neg.Type == "SyntaxError"
	}
	if isParseError {
		// Conversely, rejecting at parse time what should have failed at
		// runtime is also wrong.
		return false
	}

	var jsErr *quickjs.Error
	if !errors.As(err, &jsErr) {
		return false
	}
	name, nerr := jsErr.Value().Get("name")
	if nerr != nil {
		return false
	}
	return name.String() == neg.Type
}

// Negative is aliased so the helper above reads naturally.
type Negative = conformance.Negative

// summarize shortens an error for a one-line report.
func summarize(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:157] + "..."
	}
	return s
}
