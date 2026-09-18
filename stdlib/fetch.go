package stdlib

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
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
	// Loop is where a response is delivered. Without one the request is made
	// and waited for before fetch returns, which blocks everything else; that
	// is fine for a script that is the only thing running and wrong for
	// anything else.
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

// Network installs fetch, Headers, Request and Response.
//
// The shape is the web's: fetch returns a promise for a Response, whose text,
// json, arrayBuffer and bytes methods return promises of their own. What is
// missing is what a runtime without streams cannot do -- a body arrives whole
// rather than in pieces -- and the redirect and cache options, which the host's
// client decides.
func Network(rt *quickjs.Runtime, cfg *Fetch) error {
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
		if register.IsFunction() {
			spec.abort = make(chan struct{})
			var once sync.Once
			cancel := func() { once.Do(func() { close(spec.abort) }) }
			if _, err := register.Call(cancel); err != nil {
				p.RejectError(err)
				return p
			}
		}
		send := func() (*fetchResult, error) { return doFetch(client, cfg, spec, limit) }
		if cfg.Loop == nil {
			res, err := send()
			deliver(r, p, res, err)
			return p
		}
		cfg.Loop.Begin()
		go func() {
			defer cfg.Loop.Done()
			res, err := send()
			cfg.Loop.Post(func() { deliver(r, p, res, err) })
		}()
		return p
	}); err != nil {
		return err
	}

	api, err := evalWithHost(rt, "<fetch>", fetchJS, host)
	if err != nil {
		return err
	}
	for _, name := range []string{"fetch", "Headers", "Request", "Response"} {
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
	// abort is closed when the script's AbortSignal fires, which cancels the
	// request wherever it has got to.
	abort chan struct{}
}

// fetchResult is a response reduced to plain Go data.
type fetchResult struct {
	status     int
	statusText string
	url        string
	headers    [][2]string
	body       []byte
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
func doFetch(client *http.Client, cfg *Fetch, spec *requestSpec, limit int64) (*fetchResult, error) {
	var body io.Reader
	if spec.hasBody {
		body = strings.NewReader(string(spec.body))
	}
	req, err := http.NewRequest(spec.method, spec.url, body)
	if err != nil {
		return nil, err
	}
	for _, h := range spec.headers {
		req.Header.Add(h[0], h[1])
	}
	if spec.abort != nil {
		ctx, cancel := context.WithCancel(req.Context())
		go func() {
			select {
			case <-spec.abort:
				cancel()
			case <-ctx.Done():
			}
		}()
		req = req.WithContext(ctx)
	}
	if cfg.Allow != nil {
		if err := cfg.Allow(req); err != nil {
			return nil, err
		}
	}

	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	reader := io.Reader(res.Body)
	if limit >= 0 {
		reader = io.LimitReader(res.Body, limit)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	out := &fetchResult{
		status:     res.StatusCode,
		statusText: strings.TrimSpace(strings.TrimPrefix(res.Status, res.Proto)),
		url:        res.Request.URL.String(),
		body:       data,
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
	if err := errors.Join(
		o.Set("status", res.status),
		o.Set("statusText", res.statusText),
		o.Set("url", res.url),
		o.Set("headers", headers),
		o.Set("body", rt.NewBytes(res.body)),
	); err != nil {
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

  // A body is held as bytes, and read once: a second read is an error, as it
  // is on the web, because there is nothing left to read.
  class Body {
    constructor(bytes, headers) {
      Object.defineProperty(this, "_bytes", {value: bytes, writable: true});
      Object.defineProperty(this, "_used", {value: false, writable: true});
      this.headers = headers;
    }
    get bodyUsed() { return this._used; }
    _take() {
      if (this._used) throw new TypeError("the body has already been read");
      this._used = true;
      return this._bytes === null || this._bytes === undefined
        ? new Uint8Array(0) : this._bytes;
    }
    async arrayBuffer() {
      const b = this._take();
      return b.buffer.slice(b.byteOffset, b.byteOffset + b.byteLength);
    }
    async bytes() { return this._take(); }
    async text() { return new TextDecoder().decode(this._take()); }
    async json() { return JSON.parse(new TextDecoder().decode(this._take())); }
    async blob() { throw new TypeError("blobs are not supported"); }
    async formData() { throw new TypeError("form data is not supported"); }
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
        method: this.method, headers: this.headers, body: this._bytes,
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
      const copy = new Response(this._bytes, {
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

  // A body given as text becomes bytes, and says what it is unless told.
  function encodeBody(body, headers) {
    if (body === null || body === undefined) return null;
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

  async function fetch(input, init = {}) {
    const request = input instanceof Request && init === undefined
      ? input : new Request(input, init);
    const signal = request.signal;
    if (signal && signal.aborted) {
      throw signal.reason || new Error("This operation was aborted");
    }

    const sent = host.send({
      method: request.method,
      url: request.url,
      headers: [...request.headers._list],
      body: request._bytes,
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

    const response = new Response(raw.body, {
      status: raw.status,
      statusText: raw.statusText,
      headers: raw.headers,
      url: raw.url,
    });
    return response;
  }

  return {fetch, Headers, Request, Response};
})`
