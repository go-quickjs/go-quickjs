package stdlib

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"io"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// maxInflated bounds what one decompression may produce, so that a small input
// cannot ask for all the memory there is. A script that means to decompress
// more than this should do it in pieces.
const maxInflated = 1 << 30

// Compression installs the web's CompressionStream and DecompressionStream and
// node's zlib module.
//
// Compressing is arithmetic -- it reads bytes and writes bytes -- so it needs
// no capability, and the formats are the three that HTTP uses: gzip, zlib's
// deflate, and raw deflate.
//
//	const packed = await new Response(
//	    stream.pipeThrough(new CompressionStream("gzip"))).arrayBuffer()
//
//	import {gzipSync, gunzipSync} from "zlib"
func Compression(rt *quickjs.Runtime) error {
	c := &compressHost{rt: rt}
	host := rt.NewObject()
	if err := setAll(host, map[string]any{
		"deflate": c.deflate,
		"inflate": c.inflate,
		"packer":  c.packer,
	}); err != nil {
		return err
	}

	api, err := evalWithHost(rt, "<compress>", compressJS, host)
	if err != nil {
		return err
	}
	for _, name := range []string{"CompressionStream", "DecompressionStream"} {
		v, err := api.Get(name)
		if err != nil {
			return err
		}
		if err := rt.Set(name, v); err != nil {
			return err
		}
	}
	zlibAPI, err := api.Get("zlib")
	if err != nil {
		return err
	}
	exports, err := moduleExports(rt, zlibAPI)
	if err != nil {
		return err
	}
	if err := rt.SetModuleValues("zlib", exports); err != nil {
		return err
	}
	return rt.SetModuleValues("node:zlib", exports)
}

type compressHost struct {
	rt *quickjs.Runtime
}

// writerFor is the one place a format name becomes a compressor. The names are
// the web's, which are also the three framings HTTP uses.
func writerFor(format string, out io.Writer) (io.WriteCloser, error) {
	switch format {
	case "gzip":
		return gzip.NewWriter(out), nil
	case "deflate":
		return zlib.NewWriter(out), nil
	case "deflate-raw":
		return flate.NewWriter(out, flate.DefaultCompression)
	}
	return nil, errors.New("unsupported compression format: " + format)
}

func readerFor(format string, in io.Reader) (io.ReadCloser, error) {
	switch format {
	case "gzip":
		return gzip.NewReader(in)
	case "deflate":
		return zlib.NewReader(in)
	case "deflate-raw":
		return io.NopCloser(flate.NewReader(in)), nil
	}
	return nil, errors.New("unsupported compression format: " + format)
}

func (c *compressHost) deflate(format string, data quickjs.Value) (quickjs.Value, error) {
	b, ok := data.Bytes()
	if !ok {
		return quickjs.Value{}, c.rt.Throw(c.rt.NewError("TypeError",
			"the data must be a typed array or an ArrayBuffer"))
	}
	var out bytes.Buffer
	w, err := writerFor(format, &out)
	if err != nil {
		return quickjs.Value{}, c.rt.Throw(c.rt.NewError("TypeError", err.Error()))
	}
	if _, err := w.Write(b); err != nil {
		return quickjs.Value{}, err
	}
	if err := w.Close(); err != nil {
		return quickjs.Value{}, err
	}
	return c.rt.NewBytes(out.Bytes()), nil
}

func (c *compressHost) inflate(format string, data quickjs.Value) (quickjs.Value, error) {
	b, ok := data.Bytes()
	if !ok {
		return quickjs.Value{}, c.rt.Throw(c.rt.NewError("TypeError",
			"the data must be a typed array or an ArrayBuffer"))
	}
	r, err := readerFor(format, bytes.NewReader(b))
	if err != nil {
		return quickjs.Value{}, c.rt.Throw(c.rt.NewError("Error", err.Error()))
	}
	defer r.Close()
	// One more byte than the limit, so that reaching it is told apart from
	// happening to end there.
	out, err := io.ReadAll(io.LimitReader(r, maxInflated+1))
	if err != nil {
		return quickjs.Value{}, c.rt.Throw(c.rt.NewError("Error",
			"the data could not be decompressed: "+err.Error()))
	}
	if len(out) > maxInflated {
		return quickjs.Value{}, c.rt.Throw(c.rt.NewError("RangeError",
			"the decompressed data is larger than this runtime will produce at once"))
	}
	return c.rt.NewBytes(out), nil
}

// packer hands back a compressor that keeps its state between calls, which is
// what lets a stream be compressed as it goes rather than gathered first.
//
// There is no counterpart for decompression: a decompressor has to be pulled
// rather than pushed, and the stream that feeds it cannot be pulled from
// another goroutine without the runtime going with it.
func (c *compressHost) packer(format string) (quickjs.Value, error) {
	var out bytes.Buffer
	w, err := writerFor(format, &out)
	if err != nil {
		return quickjs.Value{}, c.rt.Throw(c.rt.NewError("TypeError", err.Error()))
	}
	closed := false
	take := func() quickjs.Value {
		b := append([]byte(nil), out.Bytes()...)
		out.Reset()
		return c.rt.NewBytes(b)
	}
	o := c.rt.NewObject()
	err = setAll(o, map[string]any{
		"write": func(data quickjs.Value) (quickjs.Value, error) {
			if closed {
				return quickjs.Value{}, c.rt.Throw(c.rt.NewError("TypeError",
					"this compressor has already finished"))
			}
			b, ok := data.Bytes()
			if !ok {
				return quickjs.Value{}, c.rt.Throw(c.rt.NewError("TypeError",
					"a compression stream takes bytes"))
			}
			if _, err := w.Write(b); err != nil {
				return quickjs.Value{}, err
			}
			return take(), nil
		},
		"close": func() (quickjs.Value, error) {
			if closed {
				return c.rt.NewBytes(nil), nil
			}
			closed = true
			if err := w.Close(); err != nil {
				return quickjs.Value{}, err
			}
			return take(), nil
		},
	})
	if err != nil {
		return quickjs.Value{}, err
	}
	return o, nil
}

// compressJS is the two stream classes and the zlib module.
const compressJS = `(function (host) {
  "use strict";

  const asBytes = (chunk) => {
    if (chunk instanceof Uint8Array) return chunk;
    if (ArrayBuffer.isView(chunk)) {
      return new Uint8Array(chunk.buffer, chunk.byteOffset, chunk.byteLength);
    }
    if (chunk instanceof ArrayBuffer) return new Uint8Array(chunk);
    if (typeof chunk === "string") return new TextEncoder().encode(chunk);
    throw new TypeError("a compression stream takes bytes");
  };

  // A compressor is fed as the data arrives, so a stream through it never
  // holds more than what has not been written out yet.
  class CompressionStream {
    constructor(format) {
      const packer = host.packer(String(format));
      const inner = new TransformStream({
        transform(chunk, controller) {
          const out = packer.write(asBytes(chunk));
          if (out.length > 0) controller.enqueue(out);
        },
        flush(controller) {
          const out = packer.close();
          if (out.length > 0) controller.enqueue(out);
        },
      });
      this.readable = inner.readable;
      this.writable = inner.writable;
    }
  }

  // Going the other way the input is gathered first: a decompressor is pulled
  // rather than pushed, and there is nothing here to pull it.
  class DecompressionStream {
    constructor(format) {
      const name = String(format);
      const chunks = [];
      const inner = new TransformStream({
        transform(chunk) { chunks.push(asBytes(chunk)); },
        flush(controller) {
          let total = 0;
          for (const c of chunks) total += c.length;
          const all = new Uint8Array(total);
          let at = 0;
          for (const c of chunks) { all.set(c, at); at += c.length; }
          const out = host.inflate(name, all);
          if (out.length > 0) controller.enqueue(out);
        },
      });
      this.readable = inner.readable;
      this.writable = inner.writable;
    }
  }

  // --- zlib -----------------------------------------------------------------

  const wrap = (bytes) => typeof Buffer !== "undefined" ? Buffer.from(bytes) : bytes;
  const input = (data) => typeof data === "string"
    ? new TextEncoder().encode(data) : asBytes(data);

  // node's functions take a callback; the work is not slow enough to need one,
  // but code written for node passes one, so both forms are here.
  function pair(name, fn) {
    const sync = (data, options) => wrap(fn(input(data)));
    const async = (data, options, callback) => {
      if (typeof options === "function") { callback = options; options = undefined; }
      queueMicrotask(() => {
        try { callback(null, sync(data, options)); }
        catch (e) { callback(e); }
      });
    };
    return {[name + "Sync"]: sync, [name]: async};
  }

  const zlib = Object.assign(
    {},
    pair("gzip", (b) => host.deflate("gzip", b)),
    pair("gunzip", (b) => host.inflate("gzip", b)),
    pair("deflate", (b) => host.deflate("deflate", b)),
    pair("inflate", (b) => host.inflate("deflate", b)),
    pair("deflateRaw", (b) => host.deflate("deflate-raw", b)),
    pair("inflateRaw", (b) => host.inflate("deflate-raw", b)),
    {
      // unzip accepts either of the two framings, which is what makes it
      // useful for something that arrived over HTTP.
      unzipSync(data) {
        const bytes = input(data);
        try { return wrap(host.inflate("gzip", bytes)); }
        catch (e) { return wrap(host.inflate("deflate", bytes)); }
      },
      constants: {
        Z_NO_COMPRESSION: 0, Z_BEST_SPEED: 1, Z_BEST_COMPRESSION: 9,
        Z_DEFAULT_COMPRESSION: -1,
      },
      createGzip: () => new CompressionStream("gzip"),
      createGunzip: () => new DecompressionStream("gzip"),
      createDeflate: () => new CompressionStream("deflate"),
      createInflate: () => new DecompressionStream("deflate"),
      createDeflateRaw: () => new CompressionStream("deflate-raw"),
      createInflateRaw: () => new DecompressionStream("deflate-raw"),
    });
  zlib.unzip = (data, options, callback) => {
    if (typeof options === "function") { callback = options; options = undefined; }
    queueMicrotask(() => {
      try { callback(null, zlib.unzipSync(data)); } catch (e) { callback(e); }
    });
  };
  zlib.default = zlib;

  return {CompressionStream, DecompressionStream, zlib};
})`
