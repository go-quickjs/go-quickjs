// Package conformance runs the official ECMAScript test suite, test262,
// against the engine.
//
// Each test carries a YAML frontmatter block describing how it must be run:
// which harness files to include, whether it is expected to fail and at what
// phase, whether it is a module, and which language features it needs. Honouring
// that metadata is most of the work; running the file is the easy part.
//
// The suite is not vendored. Point the runner at a checkout with the
// TEST262_DIR environment variable, or pass the path explicitly.
package conformance

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Test is one test262 case with its metadata resolved.
type Test struct {
	// Path is the file's path relative to the suite's test directory.
	Path string
	// Source is the file's contents.
	Source string
	Meta   Metadata
	// Strict says which variant this is. A test without onlyStrict or
	// noStrict runs twice, once each way.
	Strict bool
}

// Name returns a readable identifier for the test.
func (t *Test) Name() string {
	if t.Strict {
		return t.Path + " [strict]"
	}
	return t.Path + " [sloppy]"
}

// Metadata is the YAML frontmatter of a test262 file.
type Metadata struct {
	Description string
	// Includes names the harness files the test needs on top of the defaults.
	Includes []string
	// Flags controls how the test is run.
	Flags map[string]bool
	// Negative describes an expected failure.
	Negative *Negative
	// Features lists the language features the test exercises, which lets a
	// runner skip what an engine does not claim to support.
	Features []string
}

// Negative is the expected-failure declaration.
type Negative struct {
	// Phase is "parse", "resolution" or "runtime", saying when the error must
	// occur. A test expecting a parse error that instead fails at runtime has
	// not passed.
	Phase string
	// Type names the constructor of the expected error.
	Type string
}

// Suite is a loaded test262 checkout.
type Suite struct {
	Root string
	// harness caches the harness files, which every test includes and which
	// would otherwise be re-read tens of thousands of times. The mutex is
	// there because the runner works through the suite on several goroutines.
	harnessMu sync.Mutex
	harness   map[string]string
}

// Open locates a test262 checkout.
//
// The path may be given explicitly, or through TEST262_DIR; an empty result
// with a nil error means no checkout was found, which lets a test skip rather
// than fail.
func Open(dir string) (*Suite, error) {
	if dir == "" {
		dir = os.Getenv("TEST262_DIR")
	}
	// There is deliberately no search of well-known locations: the suite is
	// tens of thousands of files, so running it has to be an explicit choice
	// rather than something `go test ./...` stumbles into.
	if dir == "" {
		return nil, nil
	}
	if !isSuiteRoot(dir) {
		return nil, fmt.Errorf("conformance: %s is not a test262 checkout", dir)
	}
	return &Suite{Root: dir, harness: make(map[string]string)}, nil
}

func isSuiteRoot(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "harness", "assert.js")); err != nil {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, "test"))
	return err == nil
}

// Harness returns the contents of a harness file.
func (s *Suite) Harness(name string) (string, error) {
	s.harnessMu.Lock()
	src, ok := s.harness[name]
	s.harnessMu.Unlock()
	if ok {
		return src, nil
	}
	b, err := os.ReadFile(filepath.Join(s.Root, "harness", name))
	if err != nil {
		return "", err
	}
	s.harnessMu.Lock()
	s.harness[name] = string(b)
	s.harnessMu.Unlock()
	return string(b), nil
}

// Load walks the suite and returns every test under the given sub-paths,
// expanded into their strict and sloppy variants.
//
// A nil or empty subdirs means the whole suite.
func (s *Suite) Load(subdirs []string) ([]*Test, error) {
	roots := subdirs
	if len(roots) == 0 {
		roots = []string{""}
	}

	var out []*Test
	for _, sub := range roots {
		base := filepath.Join(s.Root, "test", sub)
		err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				// The staging directory holds proposals that are not part of
				// the standard.
				switch d.Name() {
				case "staging":
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".js") {
				return nil
			}
			// A _FIXTURE file is imported by another test, never run on its own.
			if strings.Contains(d.Name(), "_FIXTURE") {
				return nil
			}

			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(filepath.Join(s.Root, "test"), path)
			meta, err := ParseMetadata(string(b))
			if err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}

			for _, strict := range variantsOf(meta) {
				out = append(out, &Test{
					Path:   filepath.ToSlash(rel),
					Source: string(b),
					Meta:   meta,
					Strict: strict,
				})
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// variantsOf reports which strictness variants a test should run in.
func variantsOf(m Metadata) []bool {
	switch {
	case m.Flags["raw"]:
		// Raw tests are run exactly as written, with no harness and no
		// injected directive.
		return []bool{false}
	case m.Flags["onlyStrict"]:
		return []bool{true}
	case m.Flags["noStrict"], m.Flags["module"]:
		// Module code is already strict, so running it twice would be the same
		// test twice.
		return []bool{false}
	}
	return []bool{false, true}
}

// Prelude assembles the harness source a test needs before its own body.
func (s *Suite) Prelude(t *Test) (string, error) {
	if t.Meta.Flags["raw"] {
		return "", nil
	}
	var sb strings.Builder
	// assert.js and sta.js are included by every non-raw test, and
	// doneprintHandle.js by every async one.
	names := []string{"assert.js", "sta.js"}
	if t.Meta.Flags["async"] {
		names = append(names, "doneprintHandle.js")
	}
	names = append(names, t.Meta.Includes...)

	for _, name := range names {
		src, err := s.Harness(name)
		if err != nil {
			return "", fmt.Errorf("harness %s: %w", name, err)
		}
		sb.WriteString(src)
		sb.WriteByte('\n')
	}
	return sb.String(), nil
}

// Body returns the test's own source, with a strict directive prepended when
// the variant calls for one.
func (t *Test) Body() string {
	if t.Strict && !t.Meta.Flags["raw"] {
		return "\"use strict\";\n" + t.Source
	}
	return t.Source
}
