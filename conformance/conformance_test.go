package conformance_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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
// Unsupported feature tags can be exercised during implementation with, for
// example, -conformance.force-feature=Temporal.

var (
	reportPath = flag.String("conformance.report", "",
		"write the list of failing tests to this file")
	subdirFlag = flag.String("conformance.dir", "",
		"restrict the run to a comma-separated list of directories under test/")
	maxFailures = flag.Int("conformance.max-failures", 40,
		"how many failures to print before summarizing")
	workers = flag.Int("conformance.workers", 0,
		"how many tests to run at once; zero means one per core")
	allocReport = flag.Int64("conformance.alloc-report", 0,
		"log any test allocating more than this many bytes; implies one worker")
	forceFeatures = flag.String("conformance.force-feature", "",
		"run unsupported feature tags listed as comma-separated names")
	// The timeout is there to catch a test that hangs, not to hold one to a
	// speed. A few are genuine brute forces -- decodeURI walks every four-byte
	// UTF-8 sequence, better than a million of them -- and take seconds on
	// their own, so a limit close to that turns a busy machine into failures
	// that move from run to run.
	testTimeout = flag.Duration("conformance.timeout", 30*time.Second,
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
	"Atomics":                      "no shared memory",
	"Atomics.pause":                "no shared memory",
	"SharedArrayBuffer":            "no shared memory",
	"resizable-arraybuffer":        "no resizable buffers",
	"decorators":                   "decorators are not implemented",
	"import-assertions":            "no module attributes",
	"import-attributes":            "no module attributes",
	"explicit-resource-management": "no using declarations",
	"source-phase-imports":         "no source phase imports",
	"import-defer":                 "no deferred imports",
	"tail-call-optimization":       "no tail calls",
	"ShadowRealm":                  "no shadow realms",
	"legacy-regexp":                "no legacy RegExp statics",
	"error-stack-accessor":         "Error stack is an own data property",
	"immutable-arraybuffer":        "no immutable ArrayBuffers",
	"String.prototype.replaceAll":  "",
	"IsHTMLDDA":                    "no document.all emulation",
	"cross-realm":                  "no realms API",
	"Reflect.construct":            "",
	"caller":                       "no legacy caller access",
	"arguments-object":             "",
}

// A newly named difference permits a failure while documenting why; an
// unlisted failure and a stale entry both break the build.
var knownDifferences = map[string]string{}

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
		// The areas this engine targets: the language, the built-ins, and the
		// internationalization API. The rest of the suite covers host
		// integration and annexes it does not claim.
		subdirs = []string{"language", "built-ins", "intl402"}
	}

	tests, err := suite.Load(subdirs)
	if err != nil {
		t.Fatalf("loading tests: %v", err)
	}
	t.Logf("loaded %d test variants from %s", len(tests), suite.Root)
	forcedFeatures := make(map[string]bool)
	for _, feature := range strings.Split(*forceFeatures, ",") {
		if feature = strings.TrimSpace(feature); feature != "" {
			forcedFeatures[feature] = true
		}
	}

	// The tests are independent -- each gets its own Runtime, and a Runtime
	// shares nothing with another -- so they are run on as many goroutines as
	// there are cores. Serially the suite takes over an hour, which is long
	// enough that nobody measures before committing.
	type outcome struct {
		res    result
		reason string
	}
	outcomes := make([]outcome, len(tests))
	next := int64(-1)
	n := *workers
	if n <= 0 {
		n = runtime.NumCPU()
	}
	if *allocReport > 0 {
		// Attributing an allocation to a test means nothing else may be
		// running at the time.
		n = 1
	}
	var wg sync.WaitGroup
	for w := 0; w < n; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(atomic.AddInt64(&next, 1))
				if i >= len(tests) {
					return
				}
				if *allocReport > 0 {
					var before, after runtime.MemStats
					runtime.ReadMemStats(&before)
					res, reason := runOne(suite, tests[i], forcedFeatures)
					runtime.ReadMemStats(&after)
					if d := after.TotalAlloc - before.TotalAlloc; d > uint64(*allocReport) {
						t.Logf("ALLOC %s: %d MB", tests[i].Name(), d/(1<<20))
					}
					outcomes[i] = outcome{res, reason}
					continue
				}
				res, reason := runOne(suite, tests[i], forcedFeatures)
				outcomes[i] = outcome{res, reason}
			}
		}()
	}
	wg.Wait()

	var (
		pass, fail, skip int
		failures         []string
		byArea           = map[string]*areaStats{}
	)

	// The tally is done afterwards and in order, so that the report reads the
	// same however the work was divided.
	for i, tc := range tests {
		res, reason := outcomes[i].res, outcomes[i].reason
		area := areaOf(tc.Path)
		st := byArea[area]
		if st == nil {
			st = &areaStats{}
			byArea[area] = st
		}

		switch res {
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
			// A failure that is not one of the named differences is a
			// regression, and is reported as one.
			if _, known := knownDifferences[tc.Path]; !known {
				t.Errorf("FAIL %s: %s", tc.Name(), reason)
			}
		case resultPass:
			pass++
			st.pass++
			// A difference that is no longer one is worth knowing about too,
			// since the list is meant to say where the engine stands.
			if why, known := knownDifferences[tc.Path]; known {
				t.Errorf("%s passes now: take it off the list (%s)", tc.Name(), why)
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
func runOne(suite *conformance.Suite, tc *conformance.Test,
	forcedFeatures map[string]bool) (result, string) {
	for _, f := range tc.Meta.Features {
		if reason, unsupported := unsupportedFeatures[f]; unsupported &&
			reason != "" && !forcedFeatures[f] {
			return resultSkip, reason
		}
	}
	// CanBlockIsFalse and similar host hooks are not provided.
	if tc.Meta.Flags["CanBlockIsFalse"] || tc.Meta.Flags["CanBlockIsTrue"] {
		return resultSkip, "no agent support"
	}
	// A test that asks the host for a second realm or for an agent is asking
	// for something this host does not have, the same as a test tagged with a
	// feature the engine does not implement. Both are skipped rather than
	// counted, and both are listed as not implemented.
	if strings.Contains(tc.Source, "$262.createRealm") {
		return resultSkip, "no second realm"
	}
	if strings.Contains(tc.Source, "$262.agent") {
		return resultSkip, "no agent support"
	}

	prelude, err := suite.Prelude(tc)
	if err != nil {
		return resultSkip, err.Error()
	}

	// The stack is sized to what 400 frames can actually reach rather than to
	// the default, because the runner has one Runtime per core in flight and
	// allocates a fresh one per test: the default six megabytes of slots is
	// more than the depth limit permits anyone to use, and churning it eighty
	// thousand times over is what it takes to run the machine out of memory.
	//
	// The language is pinned as well. A program that does not say which
	// language it means is answered with the one the machine is set to, and a
	// suite whose answers depend on whose machine is running it is no test at
	// all.
	rt := quickjs.New(
		quickjs.WithMaxCallDepth(400),
		quickjs.WithStackSize(64*1024),
		quickjs.WithLocale("en-US"),
	)
	defer rt.Close()

	// The suite's async tests report completion through print.
	var printed []string
	rt.Set("print", func(s string) { printed = append(printed, s) })

	// $262 is the host object test262 expects. Only the parts this engine can
	// honestly provide are defined; a test needing createRealm or agent is
	// skipped above rather than being told a lie about what is here.
	rt.Set("detachArrayBuffer", func(rt *quickjs.Runtime, v quickjs.Value) error {
		return rt.DetachArrayBuffer(v)
	})
	rt.Set("evalScript", func(rt *quickjs.Runtime, src string) (quickjs.Value, error) {
		return rt.Eval(src)
	})
	if _, err := rt.Eval(`
		var $262 = {
			global: globalThis,
			detachArrayBuffer: detachArrayBuffer,
			evalScript: evalScript,
			gc: function () { throw new Error("gc is not supported"); },
		};
	`); err != nil {
		return resultSkip, "could not install $262: " + err.Error()
	}

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

	// The harness is prepended to the test rather than evaluated separately,
	// which is what test262's own interpreting guide asks for: a harness file
	// declares its helpers with const, and a top-level const belongs to the
	// script it appears in.
	source := prelude + tc.Body()
	if prelude != "" && tc.Strict {
		// The strict directive has to stay at the very top of the combined
		// script for it to be a directive at all.
		source = "\"use strict\";\n" + prelude + tc.Source
	}

	// A module test goes through EvalModule so that import and export are in
	// scope; the harness has already run as a script, and its globals are
	// visible because a module's environment inherits from the global object.
	var runErr error
	if tc.Meta.Flags["module"] {
		// A module cannot have a script prepended to it, so the harness is
		// evaluated on its own; a module's bindings live in an environment
		// that inherits from the global object, so it still sees them.
		if prelude != "" {
			if _, err := rt.EvalContext(ctx, prelude); err != nil {
				return resultFail, "harness failed: " + summarize(err)
			}
		}
		_, runErr = rt.EvalModuleContext(ctx, tc.Path, tc.Body())
	} else {
		_, runErr = rt.EvalContext(ctx, source)
	}
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

	if neg.Phase == "parse" {
		// A test expecting a parse error has not passed if the engine accepted
		// the source and failed later.
		if !isParseError {
			return false
		}
		return neg.Type == "SyntaxError"
	}
	if neg.Phase == "resolution" {
		// Linking a module graph happens after every module in it has parsed,
		// and what it reports is a thrown error rather than a parse failure --
		// so either shape counts, as long as it is the right kind.
		if isParseError {
			return neg.Type == "SyntaxError"
		}
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
	if name, nerr := jsErr.Value().Get("name"); nerr == nil && !name.IsUndefined() {
		return name.String() == neg.Type
	}
	// The harness's own Test262Error has no name property, so the constructor
	// is what identifies it.
	ctor, nerr := jsErr.Value().Get("constructor")
	if nerr != nil {
		return false
	}
	name, nerr := ctor.Get("name")
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
