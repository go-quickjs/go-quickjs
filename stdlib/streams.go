package stdlib

import (
	quickjs "github.com/go-quickjs/go-quickjs"
)

// Streams installs the web's streams: ReadableStream, WritableStream,
// TransformStream, the two queuing strategies, and the text streams.
//
// A stream is how a program handles something larger than memory, or something
// that arrives over time: bytes from a file, a response from a server, a
// transformation applied as the data goes past.
//
//	const upper = new TransformStream({
//	    transform(chunk, controller) { controller.enqueue(chunk.toUpperCase()) },
//	})
//	await source.pipeThrough(upper).pipeTo(sink)
//
//	for await (const chunk of response.body) { ... }
//
// All of it is script over promises and arrays -- a stream reaches nothing by
// itself, only what is plugged into either end -- so it needs no capability and
// is installed with the rest of the pure computation.
//
// What is here is the default reader, which is what nearly everything uses.
// There is no BYOB reader: it exists to let a reader supply the buffer a read
// fills, and nothing in this runtime can avoid the copy that saves.
func Streams(rt *quickjs.Runtime) error {
	api, err := evalWithHost(rt, "<streams>", streamsJS, quickjs.Value{})
	if err != nil {
		return err
	}
	names := []string{
		"ReadableStream", "ReadableStreamDefaultReader", "ReadableStreamDefaultController",
		"WritableStream", "WritableStreamDefaultWriter", "TransformStream",
		"ByteLengthQueuingStrategy", "CountQueuingStrategy",
		"TextEncoderStream", "TextDecoderStream",
	}
	exports := map[string]quickjs.Value{}
	for _, name := range names {
		v, err := api.Get(name)
		if err != nil {
			return err
		}
		if err := rt.Set(name, v); err != nil {
			return err
		}
		exports[name] = v
	}
	exports["default"] = api
	// node keeps them in a module as well, under the name the web gave them.
	if err := rt.SetModuleValues("stream/web", exports); err != nil {
		return err
	}
	return rt.SetModuleValues("node:stream/web", exports)
}

// streamsJS is the whole of it. Streams are promises and queues, and saying
// that in script is both shorter and closer to the standard than saying it in
// Go and handing the pieces over.
const streamsJS = `(function () {
  "use strict";

  const noop = () => {};

  // A promise with its settling functions kept: how a stream hands a value to
  // whoever asked for it before it had one.
  function deferred() {
    let resolve, reject;
    const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
    // A rejection that is only reported later is not an unhandled one.
    promise.catch(noop);
    return {promise, resolve, reject};
  }

  const call = (fn, self, ...args) => {
    // A method the stream calls may be missing, may throw, or may return a
    // promise; all three become a promise here so the callers need not care.
    if (typeof fn !== "function") return Promise.resolve(undefined);
    try { return Promise.resolve(fn.apply(self, args)); }
    catch (e) { return Promise.reject(e); }
  };

  // --- ReadableStream -------------------------------------------------------

  class ReadableStreamDefaultController {
    constructor(stream, source, strategy) {
      this._stream = stream;
      this._source = source;
      this._queue = [];
      this._queued = 0;
      this._hwm = strategy.highWaterMark === undefined ? 1 : Number(strategy.highWaterMark);
      this._sizeOf = typeof strategy.size === "function" ? strategy.size : () => 1;
      this._closeRequested = false;
      this._pulling = false;
      this._pullAgain = false;
      this._started = false;
    }

    get desiredSize() {
      const state = this._stream._state;
      if (state === "errored") return null;
      if (state === "closed") return 0;
      return this._hwm - this._queued;
    }

    enqueue(chunk) {
      if (this._closeRequested) throw new TypeError("this stream is already closing");
      if (this._stream._state !== "readable") {
        throw new TypeError("this stream is not readable");
      }
      const waiting = this._stream._reads.shift();
      if (waiting) {
        // Somebody is already waiting, so the chunk never joins the queue.
        waiting.resolve({value: chunk, done: false});
      } else {
        let size = 1;
        try { size = Number(this._sizeOf(chunk)); }
        catch (e) { this.error(e); throw e; }
        this._queue.push({chunk, size});
        this._queued += size;
      }
      this._pullIfNeeded();
    }

    close() {
      if (this._closeRequested) throw new TypeError("this stream is already closing");
      if (this._stream._state !== "readable") {
        throw new TypeError("this stream is not readable");
      }
      this._closeRequested = true;
      if (this._queue.length === 0) this._stream._close();
    }

    error(reason) { this._stream._error(reason); }

    _shouldPull() {
      if (this._stream._state !== "readable" || this._closeRequested) return false;
      if (!this._started) return false;
      if (this._stream._reads.length > 0) return true;
      return this.desiredSize > 0;
    }

    _pullIfNeeded() {
      if (!this._shouldPull()) return;
      if (this._pulling) { this._pullAgain = true; return; }
      this._pulling = true;
      call(this._source.pull, this._source, this).then(() => {
        this._pulling = false;
        if (this._pullAgain) {
          this._pullAgain = false;
          this._pullIfNeeded();
        }
      }, (e) => { this._pulling = false; this.error(e); });
    }

    _take() {
      const entry = this._queue.shift();
      this._queued -= entry.size;
      if (this._queue.length === 0 && this._closeRequested) this._stream._close();
      else this._pullIfNeeded();
      return entry.chunk;
    }
  }

  class ReadableStream {
    constructor(source = {}, strategy = {}) {
      this._source = source || {};
      this._state = "readable";
      this._storedError = undefined;
      this._reader = null;
      this._reads = [];
      this._closedD = deferred();
      this._controller = new ReadableStreamDefaultController(this, this._source, strategy);

      call(this._source.start, this._source, this._controller).then(() => {
        this._controller._started = true;
        this._controller._pullIfNeeded();
      }, (e) => this._error(e));
    }

    get locked() { return this._reader !== null; }

    getReader(options = {}) {
      if (options && options.mode === "byob") {
        throw new TypeError("this runtime has no byob reader; read without a mode");
      }
      if (this._reader) throw new TypeError("this stream is already locked to a reader");
      return new ReadableStreamDefaultReader(this);
    }

    cancel(reason) {
      if (this._reader) {
        return Promise.reject(new TypeError("this stream is locked to a reader"));
      }
      return this._cancel(reason);
    }

    _cancel(reason) {
      if (this._state === "closed") return Promise.resolve(undefined);
      if (this._state === "errored") return Promise.reject(this._storedError);
      this._controller._queue = [];
      this._controller._queued = 0;
      const done = call(this._source.cancel, this._source, reason);
      this._close();
      return done.then(() => undefined);
    }

    _close() {
      if (this._state !== "readable") return;
      this._state = "closed";
      for (const r of this._reads.splice(0)) r.resolve({value: undefined, done: true});
      this._closedD.resolve(undefined);
    }

    _error(reason) {
      if (this._state !== "readable") return;
      this._state = "errored";
      this._storedError = reason;
      this._controller._queue = [];
      this._controller._queued = 0;
      for (const r of this._reads.splice(0)) r.reject(reason);
      this._closedD.reject(reason);
    }

    // tee gives two streams that see the same chunks. A chunk is not copied,
    // so two branches that both write into what they are given will see each
    // other's changes; that is what the standard says too.
    tee() {
      const reader = this.getReader();
      const controllers = [];
      const cancelled = [false, false];
      const reasons = [undefined, undefined];
      let reading = false;

      const pull = () => {
        if (reading || controllers.length < 2) return undefined;
        reading = true;
        return reader.read().then(({value, done}) => {
          reading = false;
          if (done) {
            for (let i = 0; i < 2; i++) {
              if (!cancelled[i]) { try { controllers[i].close(); } catch (e) {} }
            }
            return;
          }
          for (let i = 0; i < 2; i++) {
            if (!cancelled[i]) { try { controllers[i].enqueue(value); } catch (e) {} }
          }
        }, (e) => {
          reading = false;
          for (let i = 0; i < 2; i++) if (!cancelled[i]) controllers[i].error(e);
        });
      };

      const branch = (i) => new ReadableStream({
        start: (c) => { controllers[i] = c; },
        pull,
        // The source is only cancelled once both branches have given up on it.
        cancel: (reason) => {
          cancelled[i] = true;
          reasons[i] = reason;
          if (cancelled[0] && cancelled[1]) return reader.cancel(reasons);
        },
      });
      const one = branch(0), two = branch(1);
      return [one, two];
    }

    pipeThrough(transform, options = {}) {
      if (!transform || !transform.writable || !transform.readable) {
        throw new TypeError("a transform needs a readable and a writable side");
      }
      // The promise is not returned: piping through is about the readable end,
      // and a failure shows up there.
      this.pipeTo(transform.writable, options).catch(noop);
      return transform.readable;
    }

    async pipeTo(destination, options = {}) {
      const {preventClose, preventAbort, preventCancel, signal} = options;
      const reader = this.getReader();
      const writer = destination.getWriter();
      let aborted = null;
      const onAbort = () => { aborted = signal.reason || new Error("aborted"); };
      if (signal) {
        if (signal.aborted) onAbort();
        else if (typeof signal.addEventListener === "function") {
          signal.addEventListener("abort", onAbort);
        }
      }
      try {
        for (;;) {
          if (aborted) throw aborted;
          await writer.ready;
          const {value, done} = await reader.read();
          if (done) break;
          if (aborted) throw aborted;
          await writer.write(value);
        }
        if (!preventClose) await writer.close();
      } catch (e) {
        if (!preventAbort) await writer.abort(e).catch(noop);
        if (!preventCancel) await reader.cancel(e).catch(noop);
        throw e;
      } finally {
        if (signal && typeof signal.removeEventListener === "function") {
          signal.removeEventListener("abort", onAbort);
        }
        reader.releaseLock();
        writer.releaseLock();
      }
    }

    values(options = {}) {
      const reader = this.getReader();
      return {
        next: () => reader.read(),
        async return(value) {
          if (!options.preventCancel) await reader.cancel(value).catch(noop);
          reader.releaseLock();
          return {value, done: true};
        },
        [Symbol.asyncIterator]() { return this; },
      };
    }

    // from turns anything that can be iterated -- an array, a generator, an
    // async generator -- into a stream, pulling one value at a time so that an
    // endless source stays endless rather than being drained into memory.
    static from(iterable) {
      const async = iterable[Symbol.asyncIterator];
      const iterator = async ? async.call(iterable) : iterable[Symbol.iterator]();
      return new ReadableStream({
        async pull(controller) {
          const {value, done} = await iterator.next();
          if (done) controller.close();
          else controller.enqueue(value);
        },
        async cancel(reason) {
          if (typeof iterator.return === "function") await iterator.return(reason);
        },
      });
    }
  }

  ReadableStream.prototype[Symbol.asyncIterator] = ReadableStream.prototype.values;

  class ReadableStreamDefaultReader {
    constructor(stream) {
      this._stream = stream;
      stream._reader = this;
      this._closedD = deferred();
      stream._closedD.promise.then(
        () => this._closedD.resolve(undefined),
        (e) => this._closedD.reject(e));
    }

    get closed() { return this._closedD.promise; }

    read() {
      const stream = this._stream;
      if (!stream) return Promise.reject(new TypeError("this reader has been released"));
      if (stream._controller._queue.length > 0) {
        return Promise.resolve({value: stream._controller._take(), done: false});
      }
      if (stream._state === "closed") return Promise.resolve({value: undefined, done: true});
      if (stream._state === "errored") return Promise.reject(stream._storedError);
      const request = deferred();
      stream._reads.push(request);
      stream._controller._pullIfNeeded();
      return request.promise;
    }

    cancel(reason) {
      if (!this._stream) return Promise.reject(new TypeError("this reader has been released"));
      return this._stream._cancel(reason);
    }

    releaseLock() {
      if (!this._stream) return;
      if (this._stream._reads.length > 0) {
        throw new TypeError("this reader has reads that have not finished");
      }
      this._stream._reader = null;
      this._stream = null;
      this._closedD.reject(new TypeError("this reader has been released"));
    }
  }

  // --- WritableStream -------------------------------------------------------

  class WritableStream {
    constructor(sink = {}, strategy = {}) {
      this._sink = sink || {};
      this._state = "writable";
      this._storedError = undefined;
      this._writer = null;
      this._hwm = strategy.highWaterMark === undefined ? 1 : Number(strategy.highWaterMark);
      this._sizeOf = typeof strategy.size === "function" ? strategy.size : () => 1;
      this._queued = 0;
      this._chain = Promise.resolve();
      this._readyD = null;
      this._closedD = deferred();
      this._controller = {
        error: (e) => this._fail(e),
        signal: typeof AbortController !== "undefined" ? new AbortController().signal : undefined,
      };
      this._started = call(this._sink.start, this._sink, this._controller)
        .catch((e) => { this._fail(e); });
    }

    get locked() { return this._writer !== null; }

    getWriter() {
      if (this._writer) throw new TypeError("this stream is already locked to a writer");
      return new WritableStreamDefaultWriter(this);
    }

    close() {
      if (this._writer) {
        return Promise.reject(new TypeError("this stream is locked to a writer"));
      }
      return this._close();
    }

    abort(reason) {
      if (this._writer) {
        return Promise.reject(new TypeError("this stream is locked to a writer"));
      }
      return this._abort(reason);
    }

    get _desired() {
      if (this._state === "errored") return null;
      if (this._state === "closed") return 0;
      return this._hwm - this._queued;
    }

    _ready() {
      if (this._state === "errored") return Promise.reject(this._storedError);
      if (this._desired > 0 || this._state !== "writable") return Promise.resolve(undefined);
      if (!this._readyD) this._readyD = deferred();
      return this._readyD.promise;
    }

    _wake() {
      if (this._readyD && (this._desired > 0 || this._state !== "writable")) {
        this._readyD.resolve(undefined);
        this._readyD = null;
      }
    }

    // _write puts one chunk in the queue behind whatever is already there: a
    // sink is called with one chunk at a time, and never before the last one
    // has finished.
    _write(chunk) {
      if (this._state !== "writable") {
        return Promise.reject(this._storedError ||
          new TypeError("this stream is no longer writable"));
      }
      let size = 1;
      try { size = Number(this._sizeOf(chunk)); }
      catch (e) { this._fail(e); return Promise.reject(e); }
      this._queued += size;

      const done = this._chain
        .then(() => this._started)
        .then(() => {
          if (this._state !== "writable") {
            throw this._storedError || new TypeError("this stream is no longer writable");
          }
          return call(this._sink.write, this._sink, chunk, this._controller);
        });
      this._chain = done.then(noop, noop);
      return done.then((v) => {
        this._queued -= size;
        this._wake();
        return v;
      }, (e) => {
        this._queued -= size;
        this._fail(e);
        throw e;
      });
    }

    _close() {
      if (this._state !== "writable") {
        return Promise.reject(this._storedError ||
          new TypeError("this stream is already closed"));
      }
      const done = this._chain
        .then(() => this._started)
        .then(() => call(this._sink.close, this._sink));
      this._chain = done.then(noop, noop);
      return done.then(() => {
        this._state = "closed";
        this._wake();
        this._closedD.resolve(undefined);
      }, (e) => { this._fail(e); throw e; });
    }

    _abort(reason) {
      if (this._state === "closed" || this._state === "errored") {
        return Promise.resolve(undefined);
      }
      const done = call(this._sink.abort, this._sink, reason);
      this._fail(reason);
      return done.then(() => undefined);
    }

    _fail(reason) {
      if (this._state === "errored" || this._state === "closed") return;
      this._state = "errored";
      this._storedError = reason;
      this._wake();
      this._closedD.reject(reason);
    }
  }

  class WritableStreamDefaultWriter {
    constructor(stream) {
      this._stream = stream;
      stream._writer = this;
      this._closedD = deferred();
      stream._closedD.promise.then(
        () => this._closedD.resolve(undefined),
        (e) => this._closedD.reject(e));
    }

    get closed() { return this._closedD.promise; }
    get desiredSize() {
      if (!this._stream) throw new TypeError("this writer has been released");
      return this._stream._desired;
    }
    get ready() {
      if (!this._stream) return Promise.reject(new TypeError("this writer has been released"));
      return this._stream._ready();
    }

    write(chunk) {
      if (!this._stream) return Promise.reject(new TypeError("this writer has been released"));
      return this._stream._write(chunk);
    }
    close() {
      if (!this._stream) return Promise.reject(new TypeError("this writer has been released"));
      return this._stream._close();
    }
    abort(reason) {
      if (!this._stream) return Promise.reject(new TypeError("this writer has been released"));
      return this._stream._abort(reason);
    }
    releaseLock() {
      if (!this._stream) return;
      this._stream._writer = null;
      this._stream = null;
      this._closedD.reject(new TypeError("this writer has been released"));
    }
  }

  // --- TransformStream ------------------------------------------------------

  class TransformStream {
    constructor(transformer = {}, writableStrategy = {}, readableStrategy = {}) {
      transformer = transformer || {};
      let inner = null;
      let terminated = false;

      const controller = {
        enqueue: (chunk) => inner.enqueue(chunk),
        error: (reason) => { inner.error(reason); },
        terminate: () => {
          terminated = true;
          try { inner.close(); } catch (e) {}
        },
        get desiredSize() { return inner.desiredSize; },
      };

      const readable = new ReadableStream({
        start: (c) => { inner = c; },
        // The writable side drives this one, so there is nothing to pull: a
        // transform produces output only when it is given input.
        cancel: (reason) => call(transformer.cancel, transformer, reason),
      }, readableStrategy);

      const writable = new WritableStream({
        start: () => call(transformer.start, transformer, controller),
        write: (chunk) => {
          if (typeof transformer.transform !== "function") {
            controller.enqueue(chunk);
            return undefined;
          }
          return call(transformer.transform, transformer, chunk, controller);
        },
        close: () => call(transformer.flush, transformer, controller).then(() => {
          if (!terminated) { try { inner.close(); } catch (e) {} }
        }),
        abort: (reason) => { inner.error(reason); },
      }, writableStrategy);

      this.readable = readable;
      this.writable = writable;
    }
  }

  // --- strategies -----------------------------------------------------------

  class CountQueuingStrategy {
    constructor({highWaterMark} = {highWaterMark: 1}) { this.highWaterMark = highWaterMark; }
    get size() { return () => 1; }
  }

  class ByteLengthQueuingStrategy {
    constructor({highWaterMark} = {highWaterMark: 1}) { this.highWaterMark = highWaterMark; }
    get size() { return (chunk) => chunk.byteLength; }
  }

  // --- text streams ---------------------------------------------------------

  class TextEncoderStream {
    constructor() {
      const encoder = new TextEncoder();
      const inner = new TransformStream({
        transform(chunk, controller) { controller.enqueue(encoder.encode(String(chunk))); },
      });
      this.readable = inner.readable;
      this.writable = inner.writable;
    }
    get encoding() { return "utf-8"; }
  }

  class TextDecoderStream {
    constructor(label = "utf-8", options = {}) {
      const decoder = new TextDecoder(label, options);
      const inner = new TransformStream({
        // A chunk can end in the middle of a character, so what is left over
        // waits for the next one rather than becoming a replacement.
        transform(chunk, controller) {
          const text = decoder.decode(chunk, {stream: true});
          if (text) controller.enqueue(text);
        },
        flush(controller) {
          const rest = decoder.decode();
          if (rest) controller.enqueue(rest);
        },
      });
      this.readable = inner.readable;
      this.writable = inner.writable;
    }
    get encoding() { return "utf-8"; }
  }

  return {
    ReadableStream, ReadableStreamDefaultReader, ReadableStreamDefaultController,
    WritableStream, WritableStreamDefaultWriter, TransformStream,
    ByteLengthQueuingStrategy, CountQueuingStrategy,
    TextEncoderStream, TextDecoderStream,
  };
})`
