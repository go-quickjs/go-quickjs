package stdlib

import (
	"errors"
	"io"
	"os"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// fileChunk is how much one read of a file asks for.
const fileChunk = 64 << 10

// openRead opens a file and hands back something the script can read a piece at
// a time: the file stays open, and each read is done off the runtime's
// goroutine like every other piece of work that touches a disk.
//
// This is what createReadStream is built on. The stream it produces is the
// web's, not node's -- there are no node streams here -- so it is read with
// getReader or for-await, and it can be handed to a Response.
func (f *fsHost) openRead(p string, start, end int64) (quickjs.Value, error) {
	real, err := f.resolve(p)
	if err != nil {
		return quickjs.Value{}, err
	}
	file, err := os.Open(real)
	if err != nil {
		return quickjs.Value{}, err
	}
	if start > 0 {
		if _, err := file.Seek(start, io.SeekStart); err != nil {
			file.Close()
			return quickjs.Value{}, err
		}
	}
	// end is inclusive, as node's option is.
	left := int64(-1)
	if end >= 0 {
		left = end - start + 1
		if left < 0 {
			left = 0
		}
	}

	r := &fileReader{rt: f.rt, loop: f.cfg.Loop, file: file, left: left}
	o := f.rt.NewObject()
	if err := errors.Join(
		o.Set("read", r.read),
		o.Set("close", r.close),
	); err != nil {
		r.close()
		return quickjs.Value{}, err
	}
	return o, nil
}

// fileReader is an open file being read a chunk at a time.
type fileReader struct {
	rt   *quickjs.Runtime
	loop *Loop
	file *os.File
	// left is how much of the range remains, or negative for all of it.
	left int64
	busy bool
	done bool
}

// read answers with the next chunk, or null at the end of the file.
func (r *fileReader) read() *quickjs.Promise {
	p := r.rt.NewPromise()
	if r.done {
		p.Resolve(nil)
		return p
	}
	if r.busy {
		p.RejectError(errors.New("this file is already being read"))
		return p
	}
	size := int64(fileChunk)
	if r.left >= 0 && r.left < size {
		size = r.left
	}
	if size == 0 {
		r.close()
		p.Resolve(nil)
		return p
	}

	buf := make([]byte, size)
	finish := func(n int, err error) {
		r.busy = false
		if n > 0 {
			if r.left > 0 {
				r.left -= int64(n)
			}
			p.Resolve(r.rt.NewBytes(buf[:n]))
			return
		}
		r.close()
		if err != nil && !errors.Is(err, io.EOF) {
			p.RejectError(err)
			return
		}
		p.Resolve(nil)
	}

	r.busy = true
	if r.loop == nil {
		finish(r.file.Read(buf))
		return p
	}
	loop, file := r.loop, r.file
	loop.Begin()
	go func() {
		defer loop.Done()
		n, err := file.Read(buf)
		loop.Post(func() { finish(n, err) })
	}()
	return p
}

func (r *fileReader) close() {
	if r.done {
		return
	}
	r.done = true
	r.file.Close()
}

// openWrite opens a file for writing and hands back something the script can
// write to a piece at a time, which is what createWriteStream is built on.
func (f *fsHost) openWrite(p string, appending bool) (quickjs.Value, error) {
	if err := f.writable(); err != nil {
		return quickjs.Value{}, err
	}
	real, err := f.resolve(p)
	if err != nil {
		return quickjs.Value{}, err
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if appending {
		flags = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	}
	file, err := os.OpenFile(real, flags, 0o644)
	if err != nil {
		return quickjs.Value{}, err
	}

	w := &fileWriter{rt: f.rt, loop: f.cfg.Loop, file: file}
	o := f.rt.NewObject()
	if err := errors.Join(
		o.Set("write", w.write),
		o.Set("close", w.closeValue),
	); err != nil {
		w.file.Close()
		return quickjs.Value{}, err
	}
	return o, nil
}

// fileWriter is an open file being written a chunk at a time.
type fileWriter struct {
	rt   *quickjs.Runtime
	loop *Loop
	file *os.File
	done bool
}

func (w *fileWriter) write(chunk quickjs.Value) *quickjs.Promise {
	p := w.rt.NewPromise()
	if w.done {
		p.RejectError(errors.New("this file has already been closed"))
		return p
	}
	b, ok := chunk.Bytes()
	if !ok {
		b = []byte(chunk.String())
	}
	// The bytes are copied because the write happens on another goroutine and
	// what the script holds is the script's.
	data := append([]byte(nil), b...)

	if w.loop == nil {
		_, err := w.file.Write(data)
		if err != nil {
			p.RejectError(err)
		} else {
			p.Resolve(nil)
		}
		return p
	}
	loop, file := w.loop, w.file
	loop.Begin()
	go func() {
		defer loop.Done()
		_, err := file.Write(data)
		loop.Post(func() {
			if err != nil {
				p.RejectError(err)
				return
			}
			p.Resolve(nil)
		})
	}()
	return p
}

// closeValue closes the file and says when it is closed, so that a script can
// wait for what it wrote to be on the disk.
func (w *fileWriter) closeValue() *quickjs.Promise {
	p := w.rt.NewPromise()
	if w.done {
		p.Resolve(nil)
		return p
	}
	w.done = true
	file := w.file
	if w.loop == nil {
		if err := file.Close(); err != nil {
			p.RejectError(err)
		} else {
			p.Resolve(nil)
		}
		return p
	}
	loop := w.loop
	loop.Begin()
	go func() {
		defer loop.Done()
		err := file.Close()
		loop.Post(func() {
			if err != nil {
				p.RejectError(err)
				return
			}
			p.Resolve(nil)
		})
	}()
	return p
}

// fsStreamsJS turns what the host hands over into the web's streams.
const fsStreamsJS = `(function (host) {
  "use strict";

  // createReadStream gives a ReadableStream rather than node's Readable: there
  // are no node streams in this runtime, and the web's are what everything
  // else here speaks. It can be read with for-await, piped, or handed to a
  // Response, which is what serving a file off a disk needs.
  function createReadStream(path, options = {}) {
    if (typeof options === "string") options = {encoding: options};
    const start = options.start === undefined ? 0 : Number(options.start);
    const end = options.end === undefined ? -1 : Number(options.end);
    let file = null;
    return new ReadableStream({
      start() { file = host.openRead(String(path), start, end); },
      async pull(controller) {
        const chunk = await file.read();
        if (chunk === null || chunk === undefined) controller.close();
        else controller.enqueue(chunk);
      },
      cancel() { if (file) file.close(); },
    });
  }

  function createWriteStream(path, options = {}) {
    if (typeof options === "string") options = {encoding: options};
    const appending = options.flags === "a" || options.flags === "as";
    let file = null;
    return new WritableStream({
      start() { file = host.openWrite(String(path), appending); },
      write(chunk) {
        return file.write(typeof chunk === "string"
          ? new TextEncoder().encode(chunk) : chunk);
      },
      close() { return file.close(); },
      abort() { return file.close(); },
    });
  }

  return {createReadStream, createWriteStream};
})`
