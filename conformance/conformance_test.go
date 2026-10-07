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
	"slices"
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
	// Code compiled for a debugger has an instruction at each statement
	// that no other code has, which must change nothing it does.
	debugger = flag.Bool("conformance.debugger", false,
		"run every test in a runtime made WithDebugger, its code compiled for a debugger")
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
	"source-phase-imports-module-source": "JavaScript modules have no source",
	"decorators":                         "decorators are not implemented",
	"error-stack-accessor":               "Error stack is an own accessor, as in V8",
	"caller":                             "no legacy caller access",
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
		// The areas this engine targets: the language, the built-ins, the
		// internationalization API, and Annex B's web compatibility. The rest
		// of the suite covers host integration and staged proposals.
		subdirs = []string{"language", "built-ins", "intl402", "annexB"}
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
	// The main agent can block, as a host that is not a browser's main
	// thread can; a test for one that cannot is for another kind of host.
	if tc.Meta.Flags["CanBlockIsFalse"] {
		return resultSkip, "the main agent can block"
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
	newRuntime := func() *quickjs.Runtime {
		opts := []quickjs.Option{
			quickjs.WithMaxCallDepth(400),
			quickjs.WithStackSize(64 * 1024),
			quickjs.WithLocale("en-US"),
		}
		if *debugger {
			opts = append(opts, quickjs.WithDebugger())
		}
		return quickjs.New(opts...)
	}
	rt := newRuntime()
	defer rt.Close()
	// Agents are runtimes of their own, stopped when the test is over.
	agents := newAgentPool(newRuntime)
	defer agents.stop()
	if err := agents.install(rt); err != nil {
		return resultSkip, "could not install $262.agent: " + err.Error()
	}

	// The suite's async tests report completion through print.
	var printed []string
	rt.Set("print", func(s string) { printed = append(printed, s) })

	// $262 is the host object test262 expects. Only the parts this engine can
	// honestly provide are defined.
	rt.Set("detachArrayBuffer", func(rt *quickjs.Runtime, v quickjs.Value) error {
		return rt.DetachArrayBuffer(v)
	})
	rt.Set("evalScript", func(rt *quickjs.Runtime, src string) (quickjs.Value, error) {
		return rt.Eval(src)
	})
	rt.Set("abstractModuleSource", rt.AbstractModuleSource())
	// IsHTMLDDA stands for document.all. The suite's interpreting guide asks
	// that a call with nothing or with "" answer null; any other call answers
	// undefined.
	null, _ := rt.Eval("null")
	undefined, _ := rt.Eval("undefined")
	htmldda, err := rt.NewHTMLDDA(func(args ...quickjs.Value) quickjs.Value {
		if len(args) == 0 || (args[0].IsString() && args[0].String() == "") {
			return null
		}
		return undefined
	})
	if err != nil {
		return resultSkip, "could not make IsHTMLDDA: " + err.Error()
	}
	rt.Set("isHTMLDDA", htmldda)
	// createRealm makes a realm with a $262 of its own. Its functions are
	// made in it, so what they throw is its errors.
	var createRealm func() (quickjs.Value, error)
	createRealm = func() (quickjs.Value, error) {
		re, err := rt.NewRealm()
		if err != nil {
			return quickjs.Value{}, err
		}
		eval := re.Eval
		for _, g := range []struct {
			name string
			v    any
		}{
			{"evalScript", eval},
			{"createRealm", createRealm},
			{"detachArrayBuffer", func(rt *quickjs.Runtime, v quickjs.Value) error { return rt.DetachArrayBuffer(v) }},
		} {
			if err := re.Set(g.name, g.v); err != nil {
				return quickjs.Value{}, err
			}
		}
		return eval(`var $262 = {
			global: globalThis,
			evalScript: evalScript,
			createRealm: createRealm,
			detachArrayBuffer: detachArrayBuffer,
			gc: function () { throw new Error("gc is not supported"); },
		}; $262`)
	}
	rt.Set("createRealm", createRealm)
	// The tests of shared memory wait for agents with timers, which a host
	// with an event loop would have and this one keeps for them: a timer
	// calls back on the runtime's goroutine, when the runner next runs its
	// jobs. Without one, atomicsHelper.js makes its own of promise reactions,
	// which spin through the job queue for as long as the wait lasts.
	waitsOnHost := slices.Contains(tc.Meta.Includes, "atomicsHelper.js")
	if waitsOnHost {
		rt.Set("setTimeout", func(cb quickjs.Value, delay float64) {
			w := rt.StartAsyncWork()
			time.AfterFunc(time.Duration(delay*float64(time.Millisecond)), func() {
				w.Complete(func(err error) {
					if err == nil {
						cb.Call()
					}
				})
			})
		})
	}
	if _, err := rt.Eval(`
		var $262 = {
			global: globalThis,
			detachArrayBuffer: detachArrayBuffer,
			evalScript: evalScript,
			AbstractModuleSource: abstractModuleSource,
			IsHTMLDDA: isHTMLDDA,
			createRealm: createRealm,
			agent: ` + agentMain + `,
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
	// An async test that waits on the host goes on running what the host
	// finishes for it -- a timer, a waitAsync another agent settled -- until
	// it says it is done, or its time is up.
	if runErr == nil && waitsOnHost && tc.Meta.Flags["async"] {
	wait:
		for !asyncFinished(printed) {
			select {
			case <-rt.Wake():
				if _, runErr = rt.EvalContext(ctx, "undefined"); runErr != nil {
					break wait
				}
			case <-ctx.Done():
				runErr = ctx.Err()
				break wait
			}
		}
	}
	// An agent that threw fails the test, though the main agent was not
	// waiting on it to find out.
	if err := agents.err(); err != nil {
		return resultFail, summarize(err)
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

// asyncFinished reports whether an async test has printed that it is done.
func asyncFinished(printed []string) bool {
	for _, line := range printed {
		if line == "Test262:AsyncTestComplete" || strings.HasPrefix(line, "Test262:AsyncTestFailure") {
			return true
		}
	}
	return false
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
