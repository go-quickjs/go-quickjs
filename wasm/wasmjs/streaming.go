package wasmjs

import (
	"github.com/go-quickjs/go-quickjs/internal/hostaccess"
	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// compileStreaming and instantiateStreaming take a Response, or a promise of
// one, as the web's fetch gives: the runtime's Response global, which the
// standard library's fetch installs. The checks on it and their messages are
// Node's.

// streamingJS reads a Response's bytes: bytes(source) returns a promise of
// its ArrayBuffer, rejected with the TypeError of a source that is not a
// usable Response; then(promise, onFulfilled, onRejected) is the intrinsic
// then.
const streamingJS = `(function () {
  'use strict';
  const PromiseCtor = Promise, resolve = Promise.resolve, then = Promise.prototype.then;
  const apply = Reflect.apply, TypeErrorCtor = TypeError, is = Object.is;
  function inspect(v) {
    switch (typeof v) {
    case 'string':
      return "'" + v.replace(/\\/g, '\\\\').replace(/'/g, "\\'").replace(/\n/g, '\\n') + "'";
    case 'bigint':
      return v + 'n';
    case 'number':
      return is(v, -0) ? '-0' : '' + v;
    case 'symbol':
      return v.toString();
    }
    return '' + v;
  }
  function received(v) {
    if (v == null) return ' Received ' + v;
    if (typeof v === 'function') return ' Received function ' + v.name;
    if (typeof v === 'object') {
      const c = v.constructor;
      if (c && c.name) return ' Received an instance of ' + c.name;
      return ' Received [Object: null prototype] {}';
    }
    let s;
    if (typeof v === 'string') {
      s = inspect(v.length > 28 ? v.slice(0, 25) + '...' : v);
    } else {
      s = inspect(v);
      if (s.length > 28) s = s.slice(0, 25) + '...';
    }
    return ' Received type ' + typeof v + ' (' + s + ')';
  }
  function nodeError(code, msg) {
    const e = new TypeErrorCtor(msg);
    e.code = code;
    return e;
  }
  function bytes(source) {
    return apply(then, apply(resolve, PromiseCtor, [source]), [function (response) {
      const Response = globalThis.Response;
      if (typeof Response !== 'function' || !(response instanceof Response)) {
        throw nodeError('ERR_INVALID_ARG_TYPE', 'The "source" argument must be an instance of Response ' +
          'or an Promise resolving to Response.' + received(response));
      }
      const contentType = response.headers.get('Content-Type');
      if (contentType !== 'application/wasm') {
        throw nodeError('ERR_WEBASSEMBLY_RESPONSE', "WebAssembly response has unsupported MIME type '" + contentType + "'");
      }
      if (!response.ok) {
        throw nodeError('ERR_WEBASSEMBLY_RESPONSE', 'WebAssembly response has status code ' + response.status);
      }
      if (response.bodyUsed !== false) {
        throw nodeError('ERR_WEBASSEMBLY_RESPONSE', 'WebAssembly response body has already been used');
      }
      return response.arrayBuffer();
    }]);
  }
  return [bytes, function (p, f, r) { return apply(then, p, [f, r]); }];
})()`

// streamingHelpers returns the functions of streamingJS, evaluated the first
// time they are needed.
func (a *api) streamingHelpers() (bytes, then vm.Value, err error) {
	if a.streamBytes.IsUndefined() {
		v, err := hostaccess.EvalInternal(a.qrt, "node:internal/wasm_web_api", streamingJS)
		if err != nil {
			return vm.Undefined, vm.Undefined, err
		}
		arr := hostaccess.Unwrap(v)
		b, err := a.rt.GetProp(arr, a.rt.Intern("0"))
		if err != nil {
			return vm.Undefined, vm.Undefined, err
		}
		t, err := a.rt.GetProp(arr, a.rt.Intern("1"))
		if err != nil {
			return vm.Undefined, vm.Undefined, err
		}
		a.streamBytes, a.streamThen = b, t
	}
	return a.streamBytes, a.streamThen, nil
}

// streamCompile compiles the Response of source, calling done with the
// module or with what the compile was rejected with.
func (a *api) streamCompile(t thrower, source, options vm.Value, p *vm.HostPromise, done func(*moduleData)) error {
	ci, err := a.compileOptions(options)
	if err != nil {
		a.settle(p, err, nil)
		return nil
	}
	bytesFn, then, err := a.streamingHelpers()
	if err != nil {
		return err
	}
	bp, err := a.rt.Call(bytesFn, vm.Undefined, []vm.Value{source})
	if err != nil {
		a.settle(p, err, nil)
		return nil
	}
	onFulfilled := a.rt.NewFunc("", 1, func(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
		b, _ := vm.HostBufferSource(arg(args, 0))
		md, err := a.compile(t, append([]byte(nil), b...), ci, true)
		if err != nil {
			a.settle(p, err, nil)
			return vm.Undefined, nil
		}
		done(md)
		return vm.Undefined, nil
	})
	onRejected := a.rt.NewFunc("", 1, func(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
		p.Reject(arg(args, 0))
		return vm.Undefined, nil
	})
	_, err = a.rt.Call(then, vm.Undefined, []vm.Value{bp, vm.Obj(onFulfilled), vm.Obj(onRejected)})
	return err
}

// compileStreaming is WebAssembly.compileStreaming(source, options).
func (a *api) compileStreaming(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	t := a.thrower("WebAssembly.compileStreaming()")
	p := rt.NewHostPromise()
	err := a.streamCompile(t, arg(args, 0), arg(args, 1), p, func(md *moduleData) {
		p.Resolve(vm.Obj(a.moduleObject(md, a.moduleProto)))
	})
	if err != nil {
		return vm.Undefined, err
	}
	return p.Value(), nil
}

// instantiateStreaming is
// WebAssembly.instantiateStreaming(source, imports, options).
func (a *api) instantiateStreaming(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
	t := a.thrower("WebAssembly.instantiateStreaming()")
	p := rt.NewHostPromise()
	ffi := arg(args, 1)
	if !ffi.IsUndefined() && !ffi.IsObject() {
		a.settle(p, t.typeError("Argument 1 must be an object"), nil)
		return p.Value(), nil
	}
	err := a.streamCompile(t, arg(args, 0), arg(args, 2), p, func(md *moduleData) {
		a.instantiateLater(p, md, ffi)
	})
	if err != nil {
		return vm.Undefined, err
	}
	return p.Value(), nil
}
