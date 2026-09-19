package stdlib

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// Fetch describes the network access a runtime is given.
//
// The zero value allows any request the host's HTTP client would make, which is
// a great deal of authority: a script that can fetch can reach anything the
// process can reach, including whatever is listening on localhost. A host
// handing this to code it did not write should say where it may go:
//
//	stdlib.Network(rt, &stdlib.Fetch{
//	    Loop:  loop,
//	    Allow: func(req *http.Request) error {
//	        if req.URL.Host != "api.example.com" {
//	            return fmt.Errorf("%s is not allowed", req.URL.Host)
//	        }
//	        return nil
//	    },
//	})
type Fetch struct {
	// Loop is where a response is delivered, and where the rest of it arrives:
	// with a loop, fetch answers once the headers are in and the body is read
	// a chunk at a time as the script asks for it. Without one the request is
	// made and waited for in full before fetch returns, which blocks
	// everything else; that is fine for a script that is the only thing
	// running and wrong for anything else.
	Loop *Loop
	// Client makes the requests. Nil uses a client with a sensible timeout
	// rather than http.DefaultClient, which has none.
	Client *http.Client
	// Allow is asked about every request before it is made. Returning an error
	// refuses it, and the script sees the refusal as a failed fetch.
	Allow func(req *http.Request) error
	// MaxBodyBytes caps how much of a response is read. Zero means 32 MB; a
	// negative value means no limit at all.
	MaxBodyBytes int64
}

// Network installs Headers, Request and Response, and fetch.
//
// The three types are data -- a Request is a method, a URL and some bytes --
// so they need no capability and a nil cfg installs those alone, which is what
// a runtime that serves but may not fetch wants. fetch itself reaches the
// network, and is installed only when a host has said what it may reach.
//
// The shape is the web's: fetch returns a promise for a Response, whose text,
// json, arrayBuffer and bytes methods return promises of their own, and whose
// body is a stream read as the answer arrives. What is missing is the redirect
// and cache options, which the host's client decides.
func Network(rt *quickjs.Runtime, cfg *Fetch) error {
	typesOnly := cfg == nil
	if cfg == nil {
		cfg = &Fetch{}
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	limit := cfg.MaxBodyBytes
	if limit == 0 {
		limit = 32 << 20
	}

	host := rt.NewObject()
	if err := host.Set("send", func(r *quickjs.Runtime, req quickjs.Value, register quickjs.Value) *quickjs.Promise {
		p := r.NewPromise()
		if typesOnly {
			p.RejectError(errors.New("network access is not allowed"))
			return p
		}
		// Everything the request needs is read out of the runtime here, on the
		// goroutine that owns it, so that the sending has nothing to do with
		// JavaScript values at all.
		spec, err := readRequest(req)
		if err != nil {
			p.RejectError(err)
			return p
		}
		// The script is given a way to cancel what is about to be sent, which
		// it hangs on its abort signal. Cancelling stops the request wherever
		// it has got to rather than merely ignoring the answer.
		spec.ctx, spec.cancel = context.WithCancel(context.Background())
		if register.IsFunction() {
			if _, err := register.Call(func() { spec.cancel() }); err != nil {
				spec.cancel()
				p.RejectError(err)
				return p
			}
		}
		if cfg.Loop == nil {
			res, err := doFetch(client, cfg, spec, limit, nil)
			deliver(r, p, res, err)
			return p
		}
		loop := cfg.Loop
		loop.Begin()
		go func() {
			defer loop.Done()
			res, err := doFetch(client, cfg, spec, limit, loop)
			loop.Post(func() { deliver(r, p, res, err) })
		}()
		return p
	}); err != nil {
		return err
	}

	api, err := evalWithHost(rt, "<fetch>", fetchJS, host)
	if err != nil {
		return err
	}
	names := []string{"Headers", "Request", "Response"}
	if !typesOnly {
		names = append(names, "fetch")
	}
	for _, name := range names {
		v, err := api.Get(name)
		if err != nil {
			return err
		}
		if err := rt.Set(name, v); err != nil {
			return err
		}
	}
	return nil
}

// requestSpec is a request reduced to plain Go data.
type requestSpec struct {
	method  string
	url     string
	headers [][2]string
	body    []byte
	hasBody bool
	// ctx is cancelled when the script's AbortSignal fires, which stops the
	// request wherever it has got to. Cancelling it is also how a finished
	// request lets go of what it was holding.
	ctx    context.Context
	cancel context.CancelFunc
}

// fetchResult is a response reduced to plain Go data.
//
// The body is either the whole of it or a reader still attached to the
// connection, depending on whether there is a loop to deliver the rest on.
type fetchResult struct {
	status     int
	statusText string
	url        string
	headers    [][2]string
	body       []byte
	stream     *bodyReader
}

// readRequest copies what a request says out of the runtime.
func readRequest(req quickjs.Value) (*requestSpec, error) {
	spec := &requestSpec{method: "GET"}
	if m, err := req.Get("method"); err == nil && m.Kind() == quickjs.KindString {
		spec.method = strings.ToUpper(m.String())
	}
	u, err := req.Get("url")
	if err != nil || u.Kind() != quickjs.KindString {
		return nil, errors.New("the request has no url")
	}
	spec.url = u.String()

	headers, err := req.Get("headers")
	if err == nil && headers.IsArray() {
		for i := 0; i < headers.Len(); i++ {
			pair, err := headers.Index(i)
			if err != nil || !pair.IsArray() || pair.Len() != 2 {
				continue
			}
			name, _ := pair.Index(0)
			value, _ := pair.Index(1)
			spec.headers = append(spec.headers, [2]string{name.String(), value.String()})
		}
	}
	if body, err := req.Get("body"); err == nil && !body.IsNullish() {
		if b, ok := body.Bytes(); ok {
			spec.body = append([]byte(nil), b...)
		} else {
			spec.body = []byte(body.String())
		}
		spec.hasBody = true
	}
	return spec, nil
}

// doFetch makes the request. It touches no JavaScript value at all, which is
// what lets it run on another goroutine.
//
// With a loop to deliver on, it returns as soon as the headers have arrived and
// leaves the body attached to the connection, so that a script reading a
// response a chunk at a time is reading the network rather than a copy of it.
func doFetch(client *http.Client, cfg *Fetch, spec *requestSpec, limit int64, loop *Loop) (*fetchResult, error) {
	var body io.Reader
	if spec.hasBody {
		body = strings.NewReader(string(spec.body))
	}
	req, err := http.NewRequestWithContext(spec.ctx, spec.method, spec.url, body)
	if err != nil {
		spec.cancel()
		return nil, err
	}
	for _, h := range spec.headers {
		req.Header.Add(h[0], h[1])
	}
	if cfg.Allow != nil {
		if err := cfg.Allow(req); err != nil {
			spec.cancel()
			return nil, err
		}
	}

	res, err := client.Do(req)
	if err != nil {
		spec.cancel()
		return nil, err
	}

	out := &fetchResult{
		status:     res.StatusCode,
		statusText: strings.TrimSpace(strings.TrimPrefix(res.Status, res.Proto)),
		url:        res.Request.URL.String(),
	}
	if loop == nil {
		// Nothing to deliver the rest on, so the rest is read now.
		defer spec.cancel()
		defer res.Body.Close()
		reader := io.Reader(res.Body)
		if limit >= 0 {
			reader = io.LimitReader(res.Body, limit)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			return nil, err
		}
		out.body = data
	} else {
		// The request is still going: what holds it is cancelled when the body
		// has been read or given up on.
		out.stream = &bodyReader{
			body: res.Body, loop: loop, left: limit, release: spec.cancel,
		}
	}
	// The status line's text is what follows the code, which Go keeps whole.
	if i := strings.IndexByte(res.Status, ' '); i >= 0 {
		out.statusText = res.Status[i+1:]
	}
	names := make([]string, 0, len(res.Header))
	for name := range res.Header {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, v := range res.Header[name] {
			out.headers = append(out.headers, [2]string{name, v})
		}
	}
	return out, nil
}

// bodyReader is a response body still on the connection, read one chunk at a
// time as the script asks for them.
//
// Each read is a goroutine that settles a promise on the loop, so the reading
// is driven by the stream that wants it: nothing arrives faster than it is
// taken, and nothing is held in memory but the chunk in hand. Nothing here
// holds the loop open by itself, so a program that stops reading a body stops
// waiting for it.
type bodyReader struct {
	rt      *quickjs.Runtime
	loop    *Loop
	body    io.ReadCloser
	release func()
	// left is how much more may be read, or negative for no limit.
	left int64
	// ended is what the connection said when it last had nothing more, kept
	// for the next read: a read can return a chunk and the end together.
	ended error
	busy  bool
	done  bool
}

// chunkSize is how much one read asks for. Large enough that a big body is not
// thousands of promises, small enough that a small one is not a large
// allocation.
const chunkSize = 32 << 10

// read answers with the next chunk, or null when the body has ended.
func (b *bodyReader) read() *quickjs.Promise {
	p := b.rt.NewPromise()
	if b.done {
		p.Resolve(nil)
		return p
	}
	if b.ended != nil {
		err := b.ended
		b.finish()
		if errors.Is(err, io.EOF) {
			p.Resolve(nil)
		} else {
			p.RejectError(err)
		}
		return p
	}
	if b.busy {
		p.RejectError(errors.New("this body is already being read"))
		return p
	}
	size := int64(chunkSize)
	if b.left >= 0 && b.left < size {
		size = b.left
	}
	if size == 0 {
		// The limit has been reached, which is the end as far as the script is
		// concerned; what is left of the connection is dropped.
		b.finish()
		p.Resolve(nil)
		return p
	}

	b.busy = true
	buf := make([]byte, size)
	loop, body := b.loop, b.body
	loop.Begin()
	go func() {
		defer loop.Done()
		n, err := body.Read(buf)
		loop.Post(func() {
			b.busy = false
			if n > 0 {
				if b.left > 0 {
					b.left -= int64(n)
				}
				// A read can return a chunk and the end at once; whatever
				// ended it is the next read's answer.
				b.ended = err
				p.Resolve(b.rt.NewBytes(buf[:n]))
				return
			}
			b.finish()
			if err != nil && !errors.Is(err, io.EOF) {
				p.RejectError(err)
				return
			}
			p.Resolve(nil)
		})
	}()
	return p
}

// cancel drops the rest of the body, which closes the connection.
func (b *bodyReader) cancel() { b.finish() }

func (b *bodyReader) finish() {
	if b.done {
		return
	}
	b.done = true
	// Cancel the request before closing its body. A speculative stream read may
	// already be blocked in the transport; on Windows, Response.Body.Close can
	// wait for that read, so closing first deadlocks the event-loop goroutine
	// that is trying to cancel it.
	if b.release != nil {
		b.release()
		b.release = nil
	}
	b.body.Close()
}

// deliver settles the promise with what came back.
func deliver(rt *quickjs.Runtime, p *quickjs.Promise, res *fetchResult, err error) {
	if err != nil {
		p.RejectError(err)
		return
	}
	o := rt.NewObject()
	pairs := make([]any, 0, len(res.headers))
	for _, h := range res.headers {
		pair, err := rt.NewArray(h[0], h[1])
		if err != nil {
			p.RejectError(err)
			return
		}
		pairs = append(pairs, pair)
	}
	headers, err := rt.NewArray(pairs...)
	if err != nil {
		p.RejectError(err)
		return
	}
	fields := []error{
		o.Set("status", res.status),
		o.Set("statusText", res.statusText),
		o.Set("url", res.url),
		o.Set("headers", headers),
	}
	if res.stream != nil {
		res.stream.rt = rt
		fields = append(fields,
			o.Set("read", res.stream.read),
			o.Set("cancel", res.stream.cancel))
	} else {
		fields = append(fields, o.Set("body", rt.NewBytes(res.body)))
	}
	if err := errors.Join(fields...); err != nil {
		p.RejectError(err)
		return
	}
	p.Resolve(o)
}

// fetchJS is the web's shape over the host's one operation.
const fetchJS = `(function (host) {
  "use strict";

  const forbidden = new Set(["set-cookie2"]);

  class Headers {
    constructor(init) {
      Object.defineProperty(this, "_list", {value: [], writable: true});
      if (init instanceof Headers) {
        for (const [k, v] of init) this.append(k, v);
      } else if (Array.isArray(init)) {
        for (const pair of init) this.append(pair[0], pair[1]);
      } else if (init && typeof init === "object") {
        for (const [k, v] of Object.entries(init)) this.append(k, v);
      }
    }
    append(name, value) {
      const n = normalizeName(name);
      this._list.push([n, String(value).trim()]);
    }
    delete(name) {
      const n = normalizeName(name);
      this._list = this._list.filter(([k]) => k !== n);
    }
    get(name) {
      const n = normalizeName(name);
      const all = this._list.filter(([k]) => k === n).map(([, v]) => v);
      return all.length === 0 ? null : all.join(", ");
    }
    getSetCookie() {
      return this._list.filter(([k]) => k === "set-cookie").map(([, v]) => v);
    }
    has(name) {
      const n = normalizeName(name);
      return this._list.some(([k]) => k === n);
    }
    set(name, value) {
      const n = normalizeName(name);
      this.delete(n);
      this._list.push([n, String(value).trim()]);
    }
    forEach(fn, thisArg) {
      for (const [k, v] of this.entries()) fn.call(thisArg, v, k, this);
    }
    *entries() {
      // The names come out sorted and combined, which is what the standard
      // says an iteration over headers looks like.
      const names = [...new Set(this._list.map(([k]) => k))].sort();
      for (const n of names) yield [n, this.get(n)];
    }
    *keys() { for (const [k] of this.entries()) yield k; }
    *values() { for (const [, v] of this.entries()) yield v; }
    [Symbol.iterator]() { return this.entries(); }
    toJSON() { return Object.fromEntries(this.entries()); }
  }

  function normalizeName(name) {
    const n = String(name).toLowerCase().trim();
    if (n === "" || /[^!#$%&'*+\-.^_` + "`" + `|~0-9a-z]/.test(n)) {
      throw new TypeError("invalid header name: " + name);
    }
    return n;
  }

  const isStream = (v) =>
    typeof ReadableStream !== "undefined" && v instanceof ReadableStream;

  // A body is bytes or a stream of them, and is read once: a second read is an
  // error, as it is on the web, because there is nothing left to read.
  class Body {
    constructor(source, headers) {
      const stream = isStream(source);
      Object.defineProperty(this, "_bytes",
        {value: stream ? undefined : source, writable: true});
      Object.defineProperty(this, "_stream", {value: stream ? source : null, writable: true});
      Object.defineProperty(this, "_used", {value: false, writable: true});
      this.headers = headers;
    }
    // A body is used once something has started reading it, which for a
    // stream means a reader has been taken: the bytes may still be arriving,
    // but they are somebody else's now.
    get bodyUsed() {
      return this._used || !!(this._stream && this._stream.locked);
    }

    // body is the stream form, which is the same body seen the other way
    // round: reading it is reading the body, and a body that is not there at
    // all is null rather than an empty stream.
    get body() {
      if (this._stream) return this._stream;
      if (this._bytes === null || this._bytes === undefined) return null;
      const self = this;
      this._stream = new ReadableStream({
        pull(controller) {
          const bytes = self._take();
          if (bytes.length > 0) controller.enqueue(bytes);
          controller.close();
        },
      });
      return this._stream;
    }

    // _take is the inside of a read, and asks only whether the bytes have gone:
    // the stream form calls it from its own pull, by which time the stream is
    // locked to the reader doing the reading.
    _take() {
      if (this._used) throw new TypeError("the body has already been read");
      this._used = true;
      return this._bytes === null || this._bytes === undefined
        ? new Uint8Array(0) : this._bytes;
    }

    // _consume is _take for a body that has not arrived yet: the chunks are
    // gathered as they come, and joined once the stream ends.
    async _consume() {
      if (!this._stream || this._bytes !== undefined) return this._take();
      if (this.bodyUsed) throw new TypeError("the body has already been read");
      this._used = true;
      const chunks = [];
      let total = 0;
      for await (const chunk of this._stream) {
        chunks.push(chunk);
        total += chunk.length;
      }
      const out = new Uint8Array(total);
      let at = 0;
      for (const chunk of chunks) { out.set(chunk, at); at += chunk.length; }
      this._bytes = out;
      return out;
    }

    async arrayBuffer() {
      const b = await this._consume();
      return b.buffer.slice(b.byteOffset, b.byteOffset + b.byteLength);
    }
    async bytes() { return await this._consume(); }
    async text() { return new TextDecoder().decode(await this._consume()); }
    async json() { return JSON.parse(new TextDecoder().decode(await this._consume())); }
    async blob() {
      return new Blob([await this._consume()], {type: this.headers.get("content-type") || ""});
    }
    async formData() {
      const type = this.headers.get("content-type") || "";
      const bytes = await this._consume();
      if (/^application\/x-www-form-urlencoded/i.test(type)) {
        const form = new FormData();
        for (const [k, v] of new URLSearchParams(new TextDecoder().decode(bytes))) {
          form.append(k, v);
        }
        return form;
      }
      const boundary = /boundary=("?)([^";]+)\1/i.exec(type);
      if (!boundary) {
        throw new TypeError("this body does not say what form it is in: " + type);
      }
      return parseMultipart(bytes, boundary[2]);
    }
  }

  // A body that is still a stream is split in two rather than read, since a
  // clone of something that has not arrived cannot be a copy of it.
  function cloneBody(body) {
    if (body._bytes !== undefined) return body._bytes;
    if (!body._stream) return null;
    if (body.bodyUsed) throw new TypeError("the body has already been read");
    const [mine, theirs] = body._stream.tee();
    body._stream = mine;
    return theirs;
  }

  class Request extends Body {
    constructor(input, init = {}) {
      const from = input instanceof Request ? input : null;
      const url = from ? from.url : String(input instanceof URL ? input.href : input);
      const headers = new Headers(init.headers || (from ? from.headers : undefined));
      let body = init.body !== undefined && init.body !== null
        ? encodeBody(init.body, headers)
        : (from ? from._bytes : null);
      super(body, headers);
      this.url = url;
      this.method = String(init.method || (from ? from.method : "GET")).toUpperCase();
      this.signal = init.signal || (from ? from.signal : undefined);
      this.redirect = init.redirect || "follow";
      this.credentials = init.credentials || "same-origin";
      this.mode = init.mode || "cors";
      if ((this.method === "GET" || this.method === "HEAD") && body !== null && body !== undefined) {
        throw new TypeError("a " + this.method + " request cannot have a body");
      }
    }
    clone() {
      return new Request(this.url, {
        method: this.method, headers: this.headers, body: cloneBody(this),
        signal: this.signal,
      });
    }
  }

  class Response extends Body {
    constructor(body = null, init = {}) {
      const headers = new Headers(init.headers);
      super(body === null ? null : encodeBody(body, headers), headers);
      this.status = init.status === undefined ? 200 : Number(init.status);
      this.statusText = init.statusText === undefined ? "" : String(init.statusText);
      this.url = init.url === undefined ? "" : String(init.url);
      this.redirected = false;
      this.type = "basic";
    }
    get ok() { return this.status >= 200 && this.status < 300; }
    clone() {
      const copy = new Response(cloneBody(this), {
        status: this.status, statusText: this.statusText, headers: this.headers,
      });
      copy.url = this.url;
      return copy;
    }
    static json(data, init = {}) {
      const headers = new Headers(init.headers);
      if (!headers.has("content-type")) headers.set("content-type", "application/json");
      return new Response(JSON.stringify(data), {...init, headers});
    }
    static error() { return new Response(null, {status: 0}); }
    static redirect(url, status = 302) {
      return new Response(null, {status, headers: {location: String(url)}});
    }
  }

  // --- multipart ------------------------------------------------------------

  const CRLF = "\r\n";

  // encodeMultipart writes a form the way a browser does, since what reads it
  // at the other end was written to read that.
  function encodeMultipart(form, boundary) {
    const enc = new TextEncoder();
    const pieces = [];
    for (const [name, value] of form) {
      let head = "--" + boundary + CRLF +
        'content-disposition: form-data; name="' + escapeField(name) + '"';
      if (value instanceof Blob) {
        const filename = value.name === undefined ? "blob" : value.name;
        head += '; filename="' + escapeField(filename) + '"' + CRLF;
        head += "content-type: " + (value.type || "application/octet-stream") + CRLF + CRLF;
        pieces.push(enc.encode(head), value._bytes, enc.encode(CRLF));
      } else {
        head += CRLF + CRLF;
        pieces.push(enc.encode(head), enc.encode(String(value)), enc.encode(CRLF));
      }
    }
    pieces.push(enc.encode("--" + boundary + "--" + CRLF));
    let total = 0;
    for (const p of pieces) total += p.length;
    const out = new Uint8Array(total);
    let at = 0;
    for (const p of pieces) { out.set(p, at); at += p.length; }
    return out;
  }

  // A quote or a newline in a field name would end the header early, so they
  // are written the way the browsers settled on.
  const escapeField = (s) => String(s)
    .replace(/\r?\n|\r/g, "%0A").replace(/"/g, "%22");

  function parseMultipart(bytes, boundary) {
    const enc = new TextEncoder();
    const form = new FormData();
    const marker = enc.encode("--" + boundary);
    let at = indexOfBytes(bytes, marker, 0);
    while (at >= 0) {
      let start = at + marker.length;
      // The last boundary is followed by two dashes and nothing else.
      if (bytes[start] === 0x2d && bytes[start + 1] === 0x2d) break;
      if (bytes[start] === 0x0d) start += 2; else if (bytes[start] === 0x0a) start += 1;

      const next = indexOfBytes(bytes, marker, start);
      if (next < 0) break;
      // What lies between is the part, less the line break before the next
      // boundary.
      let stop = next;
      if (bytes[stop - 1] === 0x0a) stop--;
      if (bytes[stop - 1] === 0x0d) stop--;

      const blank = indexOfBytes(bytes, enc.encode(CRLF + CRLF), start);
      if (blank < 0 || blank > stop) { at = next; continue; }
      const head = new TextDecoder().decode(bytes.slice(start, blank));
      const body = bytes.slice(blank + 4, stop);

      let name = null, filename, type = "";
      for (const line of head.split(/\r?\n/)) {
        const at2 = line.indexOf(":");
        if (at2 < 0) continue;
        const field = line.slice(0, at2).trim().toLowerCase();
        const rest = line.slice(at2 + 1);
        if (field === "content-disposition") {
          const n = /name=("?)([^";]*)\1/i.exec(rest);
          const f = /filename=("?)([^";]*)\1/i.exec(rest);
          if (n) name = decodeField(n[2]);
          if (f) filename = decodeField(f[2]);
        } else if (field === "content-type") {
          type = rest.trim();
        }
      }
      if (name !== null) {
        if (filename !== undefined) {
          form.append(name, new File([body], filename, {type}));
        } else {
          form.append(name, new TextDecoder().decode(body));
        }
      }
      at = next;
    }
    return form;
  }

  const decodeField = (s) => s.replace(/%0A/gi, "\n").replace(/%22/gi, '"');

  // indexOfBytes is indexOf over bytes, which a Uint8Array does not have.
  function indexOfBytes(haystack, needle, from) {
    outer: for (let i = from; i + needle.length <= haystack.length; i++) {
      for (let k = 0; k < needle.length; k++) {
        if (haystack[i + k] !== needle[k]) continue outer;
      }
      return i;
    }
    return -1;
  }

  // A body given as text becomes bytes, and says what it is unless told.
  function encodeBody(body, headers) {
    if (body === null || body === undefined) return null;
    if (typeof FormData !== "undefined" && body instanceof FormData) {
      // The boundary has to be something the body does not contain, which is
      // what makes a random one the right kind of guess.
      const boundary = "----quickjs" + randomTag();
      if (!headers.has("content-type")) {
        headers.set("content-type", "multipart/form-data; boundary=" + boundary);
      }
      return encodeMultipart(body, boundary);
    }
    if (typeof Blob !== "undefined" && body instanceof Blob) {
      if (!headers.has("content-type") && body.type) headers.set("content-type", body.type);
      return body._bytes;
    }
    // A stream is passed through: what it will carry is not known yet, and
    // guessing a content type from nothing is worse than leaving it out.
    if (isStream(body)) return body;
    if (body instanceof Uint8Array) return body;
    if (ArrayBuffer.isView(body)) {
      return new Uint8Array(body.buffer, body.byteOffset, body.byteLength);
    }
    if (body instanceof ArrayBuffer) return new Uint8Array(body);
    if (body instanceof URLSearchParams) {
      if (!headers.has("content-type")) {
        headers.set("content-type", "application/x-www-form-urlencoded;charset=UTF-8");
      }
      return new TextEncoder().encode(body.toString());
    }
    if (!headers.has("content-type")) {
      headers.set("content-type", "text/plain;charset=UTF-8");
    }
    return new TextEncoder().encode(String(body));
  }

  function randomTag() {
    if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") {
      return crypto.randomUUID().replace(/-/g, "");
    }
    return String(Math.random()).slice(2) + String(Date.now());
  }

  async function fetch(input, init = {}) {
    const request = input instanceof Request && init === undefined
      ? input : new Request(input, init);
    const signal = request.signal;
    if (signal && signal.aborted) {
      throw signal.reason || new Error("This operation was aborted");
    }

    // A request body that is still arriving is gathered first: what goes out
    // is one request, and its length is part of it.
    const body = request._stream && request._bytes === undefined
      ? await request._consume() : request._bytes;

    const sent = host.send({
      method: request.method,
      url: request.url,
      headers: [...request.headers._list],
      body,
    }, (cancel) => {
      if (signal) signal.addEventListener("abort", cancel, {once: true});
    });

    // An abort races the request: whichever settles first is the answer.
    const raw = signal
      ? await Promise.race([sent, new Promise((_, reject) => {
          signal.addEventListener("abort", () => {
            reject(signal.reason || new Error("This operation was aborted"));
          }, {once: true});
        })])
      : await sent;

    // A body still on the connection is read through a stream, a chunk at a
    // time; one that arrived whole is already bytes.
    const incoming = typeof raw.read === "function"
      ? new ReadableStream({
          async pull(controller) {
            const chunk = await raw.read();
            if (chunk === null || chunk === undefined) controller.close();
            else controller.enqueue(chunk);
          },
          cancel() { raw.cancel(); },
        })
      : raw.body;

    const response = new Response(incoming, {
      status: raw.status,
      statusText: raw.statusText,
      headers: raw.headers,
      url: raw.url,
    });
    return response;
  }

  return {fetch, Headers, Request, Response};
})`
