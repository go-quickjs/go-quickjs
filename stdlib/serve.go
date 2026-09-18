package stdlib

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// Serve describes the listeners a script may open.
//
// Serving is the other half of network access, and it is not the same half:
// fetching reaches out, serving invites in. A host that wants one need not
// grant the other.
type Serve struct {
	// Loop is where a request is handled, and must be running for a server to
	// answer anything: a handler is script, and script runs on the loop.
	Loop *Loop
	// Allow is asked before a listener is opened, with the address it would
	// listen on. Nil refuses everything, which is what a zero value means.
	Allow func(address string) error
	// MaxBodyBytes caps how much of a request body is read. Zero means 32 MB.
	MaxBodyBytes int64
	// ReadTimeout and IdleTimeout bound a connection that says nothing. Zero
	// means the defaults below, which are there because a server with none is
	// a server that can be held open for ever.
	ReadTimeout time.Duration
	IdleTimeout time.Duration
}

// Servers installs serve, which starts an HTTP server whose handler is a
// function from a Request to a Response:
//
//	import {serve} from "http"
//
//	const server = serve({port: 8080}, async (request) => {
//	    const {pathname} = new URL(request.url)
//	    if (pathname === "/health") return new Response("ok")
//	    return Response.json({path: pathname})
//	})
//	console.log("listening on", server.port)
//
// The shape is the one the web has settled on -- a handler that takes a request
// and answers with a response -- rather than node's streams, which a runtime
// without streams cannot offer. A handler may be async, and what it returns is
// awaited.
//
// The server keeps the loop running until it is closed, so a program whose last
// act is to start one does not exit.
func Servers(rt *quickjs.Runtime, cfg *Serve) error {
	if cfg == nil {
		cfg = &Serve{}
	}
	s := &servers{rt: rt, cfg: cfg}

	host := rt.NewObject()
	if err := host.Set("listen", s.listen); err != nil {
		return err
	}
	api, err := evalWithHost(rt, "<http>", serveJS, host)
	if err != nil {
		return err
	}
	serve, err := api.Get("serve")
	if err != nil {
		return err
	}
	exports := map[string]any{"serve": serve, "default": map[string]any{"serve": serve}}
	if err := rt.SetModule("http", exports); err != nil {
		return err
	}
	if err := rt.SetModule("node:http", exports); err != nil {
		return err
	}
	// A global too: a script that serves is usually the whole program, and
	// asking it to import one function for that is ceremony.
	return rt.Set("serve", serve)
}

type servers struct {
	rt  *quickjs.Runtime
	cfg *Serve
}

// exchange is one request waiting for an answer.
type exchange struct {
	method  string
	url     string
	headers [][2]string
	body    []byte
	// answer carries the response back to the goroutine serving the
	// connection, which is not the one the handler runs on.
	answer chan *reply
	// chunks carries a body that arrives in pieces, one at a time: the send
	// completes when the connection has taken the chunk, which is what holds a
	// handler back from producing faster than the client reads.
	chunks chan []byte
	// gone is closed when the connection has stopped listening, so that a
	// handler still pushing into it is told rather than left waiting.
	gone chan struct{}
	// failed says the handler gave up part way through. It is written before
	// chunks is closed and read after, which is what makes it safe to share.
	failed bool
}

type reply struct {
	status  int
	headers [][2]string
	body    []byte
	// streaming says the body follows on chunks rather than being here.
	streaming bool
	err       error
}

// listen opens a listener and hands back an object the script can close.
//
// The handler is called on the loop, one request at a time as everything in a
// runtime is; the connections are served on their own goroutines and wait.
func (s *servers) listen(opts quickjs.Value, dispatch quickjs.Value) (quickjs.Value, error) {
	if s.cfg.Loop == nil {
		return quickjs.Value{}, s.rt.Throw(s.rt.NewError("Error",
			"this runtime cannot serve: it has no event loop to handle requests on"))
	}
	if !dispatch.IsFunction() {
		return quickjs.Value{}, s.rt.Throw(s.rt.NewError("TypeError",
			"serve needs a function to handle requests"))
	}

	hostname := "127.0.0.1"
	port := 0
	if opts.Kind() == quickjs.KindObject {
		if h, err := opts.Get("hostname"); err == nil && h.Kind() == quickjs.KindString {
			hostname = h.String()
		}
		if p, err := opts.Get("port"); err == nil && p.Kind() == quickjs.KindNumber {
			port = p.Int()
		}
	}
	address := net.JoinHostPort(hostname, fmt.Sprint(port))
	if s.cfg.Allow == nil {
		return quickjs.Value{}, s.rt.Throw(s.rt.NewError("Error",
			"listening is not allowed"))
	}
	if err := s.cfg.Allow(address); err != nil {
		return quickjs.Value{}, s.rt.Throw(s.rt.NewError("Error", err.Error()))
	}

	listener, err := net.Listen("tcp", address)
	if err != nil {
		return quickjs.Value{}, err
	}

	limit := s.cfg.MaxBodyBytes
	if limit == 0 {
		limit = 32 << 20
	}
	readTimeout := s.cfg.ReadTimeout
	if readTimeout == 0 {
		readTimeout = 30 * time.Second
	}
	idleTimeout := s.cfg.IdleTimeout
	if idleTimeout == 0 {
		idleTimeout = 60 * time.Second
	}

	loop := s.cfg.Loop
	// The server holds the loop open: a program whose last act is to listen is
	// not a program that has finished.
	loop.Begin()
	var closeOnce sync.Once

	srv := &http.Server{
		ReadHeaderTimeout: readTimeout,
		IdleTimeout:       idleTimeout,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(io.LimitReader(r.Body, limit))
			if err != nil {
				http.Error(w, "could not read the request", http.StatusBadRequest)
				return
			}
			ex := &exchange{
				method: r.Method,
				url:    requestURL(r),
				body:   body,
				answer: make(chan *reply, 1),
				chunks: make(chan []byte),
				gone:   make(chan struct{}),
			}
			defer close(ex.gone)
			names := make([]string, 0, len(r.Header))
			for name := range r.Header {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				for _, v := range r.Header[name] {
					ex.headers = append(ex.headers, [2]string{name, v})
				}
			}

			// The handler is script, so it runs on the loop; this goroutine
			// waits for what it decides, or for the client to give up.
			loop.Post(func() { s.handle(dispatch, ex) })
			select {
			case res := <-ex.answer:
				if res.err != nil {
					http.Error(w, "the handler failed", http.StatusInternalServerError)
					return
				}
				header := w.Header()
				for _, h := range res.headers {
					header.Add(h[0], h[1])
				}
				status := res.status
				if status == 0 {
					status = http.StatusOK
				}
				w.WriteHeader(status)
				if !res.streaming {
					w.Write(res.body)
					return
				}
				// The rest of the answer is still being made. Each piece is
				// written and flushed as it comes, so a client reading a long
				// answer sees it as it is produced.
				flusher, _ := w.(http.Flusher)
				for {
					select {
					case chunk, ok := <-ex.chunks:
						if !ok {
							if ex.failed {
								// The answer stopped part way through. The
								// connection is broken rather than finished
								// tidily, so that the client can tell the
								// difference between a short answer and a
								// whole one.
								panic(http.ErrAbortHandler)
							}
							return
						}
						if _, err := w.Write(chunk); err != nil {
							return
						}
						if flusher != nil {
							flusher.Flush()
						}
					case <-r.Context().Done():
						return
					}
				}
			case <-r.Context().Done():
			}
		}),
	}

	go func() {
		// The error of a closed listener is the ordinary end of a server.
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			loop.Post(func() {})
		}
	}()

	closer := func() {
		closeOnce.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			srv.Shutdown(ctx)
			loop.Done()
		})
	}

	out := s.rt.NewObject()
	addr := listener.Addr().(*net.TCPAddr)
	if err := errors.Join(
		out.Set("port", addr.Port),
		out.Set("hostname", addr.IP.String()),
		out.Set("url", fmt.Sprintf("http://%s", net.JoinHostPort(addr.IP.String(), fmt.Sprint(addr.Port)))),
		out.Set("close", closer),
	); err != nil {
		closer()
		return quickjs.Value{}, err
	}
	return out, nil
}

// handle gives one request to the script and takes back what it answers.
func (s *servers) handle(dispatch quickjs.Value, ex *exchange) {
	pairs := make([]any, 0, len(ex.headers))
	for _, h := range ex.headers {
		pair, err := s.rt.NewArray(h[0], h[1])
		if err != nil {
			ex.answer <- &reply{err: err}
			return
		}
		pairs = append(pairs, pair)
	}
	headers, err := s.rt.NewArray(pairs...)
	if err != nil {
		ex.answer <- &reply{err: err}
		return
	}
	req := s.rt.NewObject()
	if err := errors.Join(
		req.Set("method", ex.method),
		req.Set("url", ex.url),
		req.Set("headers", headers),
		req.Set("body", s.rt.NewBytes(ex.body)),
	); err != nil {
		ex.answer <- &reply{err: err}
		return
	}

	// done is how the script hands the answer back, which it does when the
	// handler it awaited has finished.
	answered := false
	done := func(res quickjs.Value) {
		if answered {
			return
		}
		answered = true
		ex.answer <- s.readReply(res)
	}
	// sink is the rest of an answer that is still being made: push hands over
	// one piece and says when the connection has taken it, finish says there
	// are no more.
	finished := false
	sink := s.rt.NewObject()
	if err := errors.Join(
		sink.Set("push", func(chunk quickjs.Value) *quickjs.Promise {
			p := s.rt.NewPromise()
			b, ok := chunk.Bytes()
			if !ok {
				p.RejectError(errors.New("a response body is made of bytes"))
				return p
			}
			if finished {
				p.RejectError(errors.New("this answer has already finished"))
				return p
			}
			// The bytes are copied because what the script holds is the
			// script's, and the connection reads them on another goroutine.
			data := append([]byte(nil), b...)
			loop := s.cfg.Loop
			loop.Begin()
			go func() {
				defer loop.Done()
				select {
				case ex.chunks <- data:
					loop.Post(func() { p.Resolve(nil) })
				case <-ex.gone:
					loop.Post(func() {
						p.RejectError(errors.New("the client stopped listening"))
					})
				}
			}()
			return p
		}),
		sink.Set("finish", func(failed quickjs.Value) {
			if finished {
				return
			}
			finished = true
			ex.failed = failed.Bool()
			close(ex.chunks)
		}),
	); err != nil {
		ex.answer <- &reply{err: err}
		return
	}

	if _, err := dispatch.Call(req, done, sink); err != nil {
		if !answered {
			answered = true
			ex.answer <- &reply{err: err}
		}
	}
}

// readReply copies what the script answered out of the runtime.
func (s *servers) readReply(res quickjs.Value) *reply {
	out := &reply{status: http.StatusOK}
	if res.Kind() != quickjs.KindObject {
		return &reply{err: errors.New("the handler answered with something that is not a response")}
	}
	if status, err := res.Get("status"); err == nil && status.Kind() == quickjs.KindNumber {
		out.status = status.Int()
	}
	if headers, err := res.Get("headers"); err == nil && headers.IsArray() {
		for i := 0; i < headers.Len(); i++ {
			pair, err := headers.Index(i)
			if err != nil || !pair.IsArray() || pair.Len() != 2 {
				continue
			}
			name, _ := pair.Index(0)
			value, _ := pair.Index(1)
			out.headers = append(out.headers, [2]string{name.String(), value.String()})
		}
	}
	if streaming, err := res.Get("streaming"); err == nil && streaming.Bool() {
		out.streaming = true
		return out
	}
	if body, err := res.Get("body"); err == nil && !body.IsNullish() {
		if b, ok := body.Bytes(); ok {
			out.body = append([]byte(nil), b...)
		} else {
			out.body = []byte(body.String())
		}
	}
	return out
}

// requestURL is the whole URL a request asked for, which a server sees in
// pieces.
func requestURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = "localhost"
	}
	uri := r.RequestURI
	if uri == "" {
		uri = r.URL.RequestURI()
	}
	if strings.HasPrefix(uri, "http://") || strings.HasPrefix(uri, "https://") {
		return uri
	}
	return scheme + "://" + host + uri
}

// serveJS is the shape the script sees: a handler from Request to Response.
const serveJS = `(function (host) {
  "use strict";

  function serve(options, handler) {
    // serve(handler) and serve(options, handler) are both written, and so is
    // serve({port, fetch}), which is what the platforms that have settled on
    // this shape accept.
    if (typeof options === "function" && handler === undefined) {
      handler = options;
      options = {};
    }
    options = options || {};
    if (handler === undefined && typeof options.fetch === "function") {
      handler = options.fetch;
    }
    if (typeof handler !== "function") {
      throw new TypeError("serve needs a function to handle requests");
    }

    const server = host.listen(options, (raw, done, sink) => {
      // Each request is answered on its own: a handler that throws or returns
      // nothing useful becomes a 500 rather than a connection that hangs.
      (async () => {
        try {
          const init = {method: raw.method, headers: raw.headers};
          if (raw.method !== "GET" && raw.method !== "HEAD" && raw.body.length > 0) {
            init.body = raw.body;
          }
          const request = new Request(raw.url, init);
          const response = await handler(request);
          if (!(response instanceof Response)) {
            throw new TypeError("the handler did not answer with a Response");
          }
          const headers = [...response.headers].map(([k, v]) => [k, v]);
          const body = response.body;
          if (body && typeof body.getReader === "function" && response._bytes === undefined) {
            // An answer that is still being made goes out as it is made: the
            // status and headers first, then each piece as the connection
            // takes it.
            done({status: response.status, headers, streaming: true});
            try {
              for await (const chunk of body) await sink.push(chunk);
            } catch (e) {
              // The answer stopped part way through, and the client is told
              // so: the status went out long ago, so this is the only way
              // left to say that what it has is not the whole of it.
              sink.finish(true);
              throw e;
            }
            sink.finish(false);
            return;
          }
          done({
            status: response.status,
            headers,
            body: new Uint8Array(await response.arrayBuffer()),
          });
        } catch (e) {
          if (typeof options.onError === "function") {
            try {
              const response = await options.onError(e);
              if (response instanceof Response) {
                done({
                  status: response.status,
                  headers: [...response.headers].map(([k, v]) => [k, v]),
                  body: new Uint8Array(await response.arrayBuffer()),
                });
                return;
              }
            } catch (ignored) {}
          }
          done({status: 500, headers: [["content-type", "text/plain"]],
                body: new TextEncoder().encode("Internal Server Error")});
          if (typeof console !== "undefined") console.error(e);
        }
      })();
    });
    return server;
  }

  return {serve};
})`
