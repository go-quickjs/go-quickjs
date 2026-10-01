package stdlib

import (
	"context"
	"errors"
	"fmt"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// VM installs node:vm: contexts to run code in, each a realm of its own whose
// global names are an object the caller supplies, and scripts to run there.
//
// A context has its own built-ins, as in node: an array made in one is not an
// instance of another's Array. It is not a security boundary -- node says the
// same of its own -- since a function a context is handed runs with whatever
// that function can reach. A runtime made without code generation refuses to
// compile anything here, as it refuses eval.
func VM(rt *quickjs.Runtime) error {
	host := rt.NewObject()
	// compile compiles what the script hands node:vm, which a runtime made
	// without code generation refuses, as it refuses eval.
	compile := func(r *quickjs.Runtime, src, filename string, lineOffset, columnOffset int) (*quickjs.Program, error) {
		if !r.CodeGenerationAllowed() {
			return nil, r.ThrowEvalError("code generation from strings is disabled")
		}
		return r.Compile(filename, src, quickjs.WithOffset(lineOffset, columnOffset))
	}
	// run runs src in realm, or in the realm running now when realm is nil,
	// under a timeout of its own: running out of it stops this run alone, and
	// is an Error the caller can catch, with node's code.
	run := func(r *quickjs.Runtime, realm *quickjs.Realm, src, filename string, lineOffset, columnOffset int, timeout float64) (quickjs.Value, error) {
		p, err := compile(r, src, filename, lineOffset, columnOffset)
		if err != nil {
			return quickjs.Value{}, err
		}
		ctx, d := context.Background(), time.Duration(timeout*float64(time.Millisecond))
		if d > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, d)
			defer cancel()
		}
		var v quickjs.Value
		if realm != nil {
			v, err = realm.RunProgramContext(ctx, p)
		} else {
			v, err = r.RunProgramContext(ctx, p)
		}
		if err != nil && ctx.Err() != nil && errors.Is(err, context.DeadlineExceeded) {
			e := r.NewError("Error", fmt.Sprintf("Script execution timed out after %dms", d.Milliseconds()))
			if err := e.Set("code", "ERR_SCRIPT_EXECUTION_TIMEOUT"); err != nil {
				return quickjs.Value{}, err
			}
			return quickjs.Value{}, r.Throw(e)
		}
		return v, err
	}
	for name, fn := range map[string]any{
		// newContext makes a realm, a context of sandbox unless it is
		// undefined, and hands back what runs code in it.
		"newContext": func(r *quickjs.Runtime, sandbox quickjs.Value) (quickjs.Value, error) {
			var opts []quickjs.RealmOption
			if !sandbox.IsUndefined() {
				opts = append(opts, quickjs.WithSandbox(sandbox))
			}
			re, err := r.NewRealm(opts...)
			if err != nil {
				return quickjs.Value{}, err
			}
			handle := r.NewObject()
			if err := handle.Set("global", re.Global()); err != nil {
				return quickjs.Value{}, err
			}
			err = handle.Set("run", func(r *quickjs.Runtime, src, filename string, lineOffset, columnOffset int, timeout float64) (quickjs.Value, error) {
				return run(r, re, src, filename, lineOffset, columnOffset, timeout)
			})
			return handle, err
		},
		// run runs code in the realm running now.
		"run": func(r *quickjs.Runtime, src, filename string, lineOffset, columnOffset int, timeout float64) (quickjs.Value, error) {
			return run(r, nil, src, filename, lineOffset, columnOffset, timeout)
		},
		// check compiles code, which is when a Script reports a SyntaxError.
		"check": func(r *quickjs.Runtime, src, filename string, lineOffset, columnOffset int) error {
			_, err := compile(r, src, filename, lineOffset, columnOffset)
			return err
		},
	} {
		if err := host.Set(name, fn); err != nil {
			return err
		}
	}
	api, err := evalWithHost(rt, "<vm>", vmJS, host)
	if err != nil {
		return err
	}
	exports, err := moduleExports(api)
	if err != nil {
		return err
	}
	if err := rt.SetModuleValues("vm", exports); err != nil {
		return err
	}
	return rt.SetModuleValues("node:vm", exports)
}

// vmJS is node:vm, over the host's realms.
const vmJS = `(function (host) {
  "use strict";
  // contexts maps each context -- the sandbox, or a context's own global when
  // it was made without one -- to what runs code in it.
  const contexts = new WeakMap();

  const constants = Object.freeze({
    __proto__: null,
    USE_MAIN_CONTEXT_DEFAULT_LOADER: Symbol("vm_dynamic_import_main_context_default"),
    DONT_CONTEXTIFY: Symbol("vm_context_no_contextify"),
  });

  function codedError(Ctor, code, message) {
    const e = new Ctor(message);
    e.code = code;
    return e;
  }
  function describe(v) {
    if (v === null) return "null";
    if (typeof v === "function") return "function " + v.name;
    if (typeof v === "object") return "an instance of " + (v.constructor && v.constructor.name || "Object");
    return "type " + typeof v + " (" + String(v) + ")";
  }
  function validateObject(v, name) {
    if (v === null || typeof v !== "object") {
      throw codedError(TypeError, "ERR_INVALID_ARG_TYPE",
        'The "' + name + '" argument must be of type object. Received ' + describe(v));
    }
  }
  function contextOf(v) {
    const handle = (v !== null && typeof v === "object") ? contexts.get(v) : undefined;
    if (handle === undefined) {
      throw codedError(TypeError, "ERR_INVALID_ARG_TYPE",
        'The "contextifiedObject" argument must be an vm.Context. Received ' + describe(v));
    }
    return handle;
  }
  function timeoutOf(options) {
    const timeout = options === undefined || options === null ? undefined : options.timeout;
    if (timeout === undefined) return 0;
    if (typeof timeout !== "number" || !Number.isInteger(timeout) || timeout <= 0 || timeout > 4294967295) {
      throw codedError(RangeError, "ERR_OUT_OF_RANGE",
        'The value of "options.timeout" is out of range. It must be >= 1 && <= 4294967295. Received ' + String(timeout));
    }
    return timeout;
  }

  function isContext(object) {
    validateObject(object, "object");
    return contexts.has(object);
  }

  // A context has the host's console, as a node context has one: code
  // written for node logs without asking whether it may.
  function newContext(contextObject) {
    const handle = host.newContext(contextObject);
    if (typeof console !== "undefined") {
      Object.defineProperty(handle.global, "console", { value: console, writable: true, configurable: true });
    }
    return handle;
  }

  function createContext(contextObject = {}, options) {
    if (contextObject === constants.DONT_CONTEXTIFY) {
      const handle = newContext(undefined);
      contexts.set(handle.global, handle);
      return handle.global;
    }
    validateObject(contextObject, "contextObject");
    if (contexts.has(contextObject)) return contextObject;
    contexts.set(contextObject, newContext(contextObject));
    return contextObject;
  }

  class Script {
    #code;
    #filename;
    #lineOffset;
    #columnOffset;
    constructor(code, options = {}) {
      code = String(code);
      if (typeof options === "string") options = { filename: options };
      if (options === null || typeof options !== "object") options = {};
      const { filename = "evalmachine.<anonymous>", lineOffset = 0, columnOffset = 0 } = options;
      this.#code = code;
      this.#filename = String(filename);
      this.#lineOffset = lineOffset | 0;
      this.#columnOffset = columnOffset | 0;
      host.check(code, this.#filename, this.#lineOffset, this.#columnOffset);
    }
    runInThisContext(options) {
      return host.run(this.#code, this.#filename, this.#lineOffset, this.#columnOffset, timeoutOf(options));
    }
    runInContext(contextifiedObject, options) {
      const handle = contextOf(contextifiedObject);
      return handle.run(this.#code, this.#filename, this.#lineOffset, this.#columnOffset, timeoutOf(options));
    }
    runInNewContext(contextObject, options) {
      return this.runInContext(createContext(contextObject), options);
    }
  }

  function scriptOptions(options) {
    return typeof options === "string" ? { filename: options } : options;
  }
  function runInThisContext(code, options) {
    return new Script(code, scriptOptions(options)).runInThisContext(options);
  }
  function runInContext(code, contextifiedObject, options) {
    return new Script(code, scriptOptions(options)).runInContext(contextifiedObject, options);
  }
  function runInNewContext(code, contextObject, options) {
    return new Script(code, scriptOptions(options)).runInNewContext(contextObject, options);
  }
  function createScript(code, options) {
    return new Script(code, options);
  }

  // compileFunction compiles a function whose free names are looked up in
  // each of contextExtensions, innermost last, before the context's globals.
  function compileFunction(code, params = [], options = {}) {
    code = String(code);
    if (!Array.isArray(params)) {
      throw codedError(TypeError, "ERR_INVALID_ARG_TYPE", 'The "params" argument must be an instance of Array. Received ' + describe(params));
    }
    const { filename = "", parsingContext, contextExtensions = [], lineOffset = 0, columnOffset = 0 } = options || {};
    const handle = parsingContext === undefined ? null : contextOf(parsingContext);
    const names = contextExtensions.map((_, i) => "__vmContextExtension" + i + "__");
    const src = "(function (" + names.join(", ") + ") { " +
      names.map(n => "with (" + n + ") ").join("") +
      "return function (" + params.map(String).join(", ") + ") {\n" + code + "\n}; })";
    // The body starts on the wrapper's second line, which is the caller's
    // first.
    const factory = handle === null
      ? host.run(src, String(filename), (lineOffset | 0) - 1, columnOffset | 0, 0)
      : handle.run(src, String(filename), (lineOffset | 0) - 1, columnOffset | 0, 0);
    return factory(...contextExtensions);
  }

  function measureMemory() {
    return Promise.reject(codedError(Error, "ERR_METHOD_NOT_IMPLEMENTED", "The vm.measureMemory() method is not implemented"));
  }

  return {
    Script, compileFunction, constants, createContext, createScript, isContext,
    measureMemory, runInContext, runInNewContext, runInThisContext,
  };
})`
