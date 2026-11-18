//go:build quickjs_wasm

package wasmjs

import (
	"sync"

	"github.com/go-quickjs/go-quickjs/internal/hostaccess"
	"github.com/go-quickjs/go-quickjs/internal/vm"
	"github.com/go-quickjs/go-quickjs/wasm/internal/decoder"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// compileStreaming and instantiateStreaming take a Response, or a promise of
// one, as the web's fetch gives: the runtime's Response global, which the
// standard library's fetch installs. The checks on it and their messages are
// Node's. The body is read chunk by chunk and each chunk handed to a
// decoder.Stream as it arrives, which decodes the module's sections and
// validates its function bodies as their bytes complete, on a goroutine of
// its own, so that a module that arrives slowly is decoded and validated
// while it arrives, and is ready soon after its last byte.

// streamingJS reads a Response's body: stream(source, push, setURL) returns a
// promise fulfilled once each chunk of the body has been given to push, and
// rejected with the TypeError of a source that is not a usable Response, of
// a chunk that is not bytes, or with what reading the body failed with;
// then(promise, onFulfilled, onRejected) is the intrinsic then.
const streamingJS = `(function () {
  'use strict';
  const PromiseCtor = Promise, resolve = Promise.resolve, then = Promise.prototype.then;
  const apply = Reflect.apply, TypeErrorCtor = TypeError, is = Object.is, Uint8ArrayCtor = Uint8Array;
  const getter = (C, name) => Reflect.getOwnPropertyDescriptor(C.prototype, name).get;
  const viewBuffer = getter(DataView, 'buffer'), viewOffset = getter(DataView, 'byteOffset'), viewLength = getter(DataView, 'byteLength');
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
  function pushChunk(push, chunk) {
    if (push(chunk)) return;
    // A DataView's bytes, which push does not take as they are.
    let view;
    try {
      view = new Uint8ArrayCtor(apply(viewBuffer, chunk, []), apply(viewOffset, chunk, []), apply(viewLength, chunk, []));
    } catch {
      throw nodeError('ERR_INVALID_ARG_TYPE', 'chunk must be an ArrayBufferView or an ArrayBuffer');
    }
    push(view);
  }
  async function stream(source, push, setURL) {
    const response = await apply(resolve, PromiseCtor, [source]);
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
    if (response.url) setURL(response.url);
    const body = response.body;
    if (body != null) {
      for await (const chunk of body) pushChunk(push, chunk);
    }
  }
  return [stream, function (p, f, r) { return apply(then, p, [f, r]); }];
})()`

// streamingHelpers returns the functions of streamingJS, evaluated the first
// time they are needed.
func (a *api) streamingHelpers() (stream, then vm.Value, err error) {
	if a.streamFn.IsUndefined() {
		v, err := hostaccess.EvalInternal(a.qrt, "node:internal/wasm_web_api", streamingJS)
		if err != nil {
			return vm.Undefined, vm.Undefined, err
		}
		arr := hostaccess.Unwrap(v)
		s, err := a.rt.GetProp(arr, a.rt.Intern("0"))
		if err != nil {
			return vm.Undefined, vm.Undefined, err
		}
		t, err := a.rt.GetProp(arr, a.rt.Intern("1"))
		if err != nil {
			return vm.Undefined, vm.Undefined, err
		}
		a.streamFn, a.streamThen = s, t
	}
	return a.streamFn, a.streamThen, nil
}

// streamDecode feeds the chunks of a body to a decoder.Stream on a goroutine
// that runs while there are chunks to decode, so that the runtime's goroutine
// reads the next chunk meanwhile and no goroutine waits on a body that never
// ends.
type streamDecode struct {
	s *decoder.Stream

	mu      sync.Mutex
	queue   [][]byte
	running bool
	// idle is closed when the goroutine has decoded every chunk queued.
	idle chan struct{}
}

// push queues a copy of chunk b.
func (sd *streamDecode) push(b []byte) {
	if len(b) == 0 {
		return
	}
	b = append([]byte(nil), b...)
	sd.mu.Lock()
	defer sd.mu.Unlock()
	sd.queue = append(sd.queue, b)
	if !sd.running {
		sd.running = true
		sd.idle = make(chan struct{})
		go sd.decode()
	}
}

// decode decodes the chunks queued until there are none.
func (sd *streamDecode) decode() {
	for {
		sd.mu.Lock()
		q := sd.queue
		sd.queue = nil
		if len(q) == 0 {
			sd.running = false
			close(sd.idle)
			sd.mu.Unlock()
			return
		}
		sd.mu.Unlock()
		for _, b := range q {
			sd.s.Write(b)
		}
	}
}

// finish waits for the chunks queued to be decoded and returns the module.
func (sd *streamDecode) finish() (*module.Module, *decoder.Error) {
	sd.mu.Lock()
	running, idle := sd.running, sd.idle
	sd.mu.Unlock()
	if running {
		<-idle
	}
	return sd.s.Finish()
}

// streamCompile compiles the Response of source, calling done with the
// module or with what the compile was rejected with.
func (a *api) streamCompile(t thrower, source, options vm.Value, p *vm.HostPromise, done func(*moduleData)) error {
	ci, err := a.compileOptions(options)
	if err != nil {
		a.settle(p, err, nil)
		return nil
	}
	streamFn, then, err := a.streamingHelpers()
	if err != nil {
		return err
	}
	sd := &streamDecode{s: decoder.NewStream(a.limits, 0)}
	push := a.rt.NewFunc("push", 1, func(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
		b, ok := vm.HostBufferSource(arg(args, 0))
		if !ok {
			return vm.False, nil
		}
		sd.push(b)
		return vm.True, nil
	})
	url := ""
	setURL := a.rt.NewFunc("setURL", 1, func(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
		s, err := rt.ToString(arg(args, 0))
		if err != nil {
			return vm.Undefined, err
		}
		url = s.Go()
		return vm.Undefined, nil
	})
	bp, err := a.rt.Call(streamFn, vm.Undefined, []vm.Value{source, vm.Obj(push), vm.Obj(setURL)})
	if err != nil {
		a.settle(p, err, nil)
		return nil
	}
	onFulfilled := a.rt.NewFunc("", 1, func(rt *vm.Runtime, this vm.Value, args []vm.Value) (vm.Value, error) {
		m, derr := sd.finish()
		if derr != nil {
			a.settle(p, t.compileError("%s", derr.Error()), nil)
			return vm.Undefined, nil
		}
		m.SourceURL = url
		md, err := a.compiled(t, &moduleData{m: m, ci: ci}, true)
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
