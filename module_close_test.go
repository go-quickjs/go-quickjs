package quickjs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestCloseWhileLoadingModules pins what closing a runtime from inside its
// own module loading does: from a loader -- a ModuleLoader, or an
// AsyncModuleLoader that then never answers -- from a synthetic module's
// evaluate, and from a Go function a module's body calls, reached by
// EvalModule, by an import() and by RequireModule. The call returns
// ErrClosed, nothing of the graph runs after, and nothing hangs or panics.
func TestCloseWhileLoadingModules(t *testing.T) {
	type setup func(rt *quickjs.Runtime, ran *[]string)
	syncLoader := func(rt *quickjs.Runtime, ran *[]string) {
		rt.SetModuleLoader(func(specifier, referrer string) (string, string, error) {
			if specifier == "./closes" {
				rt.Close()
				return `export {}`, "/closes", nil
			}
			return `record("` + specifier + `")`, "/" + specifier, nil
		})
	}
	asyncLoader := func(rt *quickjs.Runtime, ran *[]string) {
		rt.SetAsyncModuleLoader(func(ctx context.Context, req quickjs.ModuleRequest, done func(quickjs.LoadedModule, error)) {
			if req.Specifier == "./closes" {
				rt.Close()
				return // and never answers
			}
			go done(quickjs.LoadedModule{Source: `record("` + req.Specifier + `")`, Resolved: "/" + req.Specifier}, nil)
		})
	}
	synthetic := func(rt *quickjs.Runtime, ran *[]string) {
		rt.SetModuleLoader(func(specifier, referrer string) (string, string, error) {
			if specifier == "./closes" {
				err := rt.DefineSyntheticModule("/closes", nil, func() (map[string]any, error) {
					rt.Close()
					return nil, nil
				})
				return "", "/closes", err
			}
			return `record("` + specifier + `")`, "/" + specifier, nil
		})
	}
	body := func(rt *quickjs.Runtime, ran *[]string) {
		rt.Set("closeNow", func() { rt.Close() })
		rt.SetModuleLoader(func(specifier, referrer string) (string, string, error) {
			if specifier == "./closes" {
				return `closeNow(); record("after close")`, "/closes", nil
			}
			return `record("` + specifier + `")`, "/" + specifier, nil
		})
	}
	for name, s := range map[string]setup{"loader": syncLoader, "async loader": asyncLoader, "synthetic": synthetic, "body": body} {
		for _, via := range []string{"EvalModule", "import()", "RequireModule"} {
			t.Run(name+" via "+via, func(t *testing.T) {
				rt := quickjs.New()
				var ran []string
				s(rt, &ran)
				rt.Set("requireModule", func(spec string) (quickjs.Value, error) { return rt.RequireModule(spec, "/main.js") })
				rt.Set("record", func(what string) { ran = append(ran, what) })
				result := make(chan error, 1)
				go func() {
					var err error
					switch via {
					case "EvalModule":
						_, err = rt.EvalModule("/main.mjs", `import "./before"; import "./closes"; import "./after"; record("main");`)
					case "import()":
						_, err = rt.Eval(`import("./closes").then(() => record("imported")); record("script")`)
						if err == nil {
							_, err = rt.Eval(`record("next call")`)
						}
					case "RequireModule":
						_, err = rt.Eval(`requireModule("./closes"); record("after require")`)
					}
					result <- err
				}()
				select {
				case err := <-result:
					if !errors.Is(err, quickjs.ErrClosed) {
						t.Errorf("got %v, want ErrClosed", err)
					}
					// What ran before the close may have; nothing after it.
					for _, what := range ran {
						switch what {
						case "./before", "script":
						default:
							t.Errorf("%q ran after the runtime closed (ran %v)", what, ran)
						}
					}
				case <-time.After(5 * time.Second):
					t.Fatal("hung")
				}
			})
		}
	}
}
