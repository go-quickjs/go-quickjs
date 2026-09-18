package stdlib

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/md5"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"errors"
	"hash"
	"io"
	"math/big"

	"crypto/sha3"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// Crypto installs the "crypto" module: hashing, message authentication, key
// derivation and randomness.
//
// None of it reaches outside the process -- a hash is arithmetic -- so it needs
// no capability. Randomness comes from the reader the host gave, which is
// crypto/rand unless it said otherwise; a host that wants a program to run the
// same way twice passes a reader that repeats.
//
//	import {createHash, createHmac, randomBytes} from "crypto"
//	createHash("sha256").update("hello").digest("hex")
//	createHmac("sha256", key).update(body).digest("base64")
//
// The primitives are Go's, so they are the ones Go's own users get: constant
// time where it matters, and no reimplementation of a cipher in script.
func Crypto(rt *quickjs.Runtime, random io.Reader) error {
	if random == nil {
		random = rand.Reader
	}
	c := &cryptoHost{rt: rt, random: random}

	host := rt.NewObject()
	if err := setAll(host, map[string]any{
		"createHash":      c.createHash,
		"createHmac":      c.createHmac,
		"randomBytes":     c.randomBytes,
		"randomInt":       c.randomInt,
		"timingSafeEqual": c.timingSafeEqual,
		"pbkdf2":          c.pbkdf2,
		"hkdf":            c.hkdf,
		"hashes":          hashNames,
	}); err != nil {
		return err
	}

	api, err := evalWithHost(rt, "<crypto>", cryptoJS, host)
	if err != nil {
		return err
	}
	exports, err := moduleExports(rt, api)
	if err != nil {
		return err
	}
	if err := rt.SetModuleValues("crypto", exports); err != nil {
		return err
	}
	return rt.SetModuleValues("node:crypto", exports)
}

type cryptoHost struct {
	rt     *quickjs.Runtime
	random io.Reader
}

// hashNames are the algorithms this runtime knows, under the names node uses.
var hashNames = []string{
	"md5", "sha1", "sha224", "sha256", "sha384", "sha512",
	"sha512-224", "sha512-256", "sha3-224", "sha3-256", "sha3-384", "sha3-512",
}

// newHash is the one place an algorithm name becomes a hash.
func newHash(name string) (hash.Hash, error) {
	switch canonicalHashName(name) {
	case "md5":
		return md5.New(), nil
	case "sha1":
		return sha1.New(), nil
	case "sha224":
		return sha256.New224(), nil
	case "sha256":
		return sha256.New(), nil
	case "sha384":
		return sha512.New384(), nil
	case "sha512":
		return sha512.New(), nil
	case "sha512-224":
		return sha512.New512_224(), nil
	case "sha512-256":
		return sha512.New512_256(), nil
	case "sha3-224":
		return sha3.New224(), nil
	case "sha3-256":
		return sha3.New256(), nil
	case "sha3-384":
		return sha3.New384(), nil
	case "sha3-512":
		return sha3.New512(), nil
	}
	return nil, errors.New("unsupported digest algorithm: " + name)
}

// canonicalHashName accepts the spellings that are written: node's "sha256",
// the web's "SHA-256", and openssl's "SHA512-256".
func canonicalHashName(name string) string {
	out := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		ch := name[i]
		if ch >= 'A' && ch <= 'Z' {
			ch += 'a' - 'A'
		}
		// A dash separates a size from a family in sha3-256 and sha512-256, but
		// joins the two halves of the web's SHA-256; the difference is whether
		// what follows is the whole of the rest.
		if ch == '-' && i+1 < len(name) && name[i+1] >= '0' && name[i+1] <= '9' &&
			len(out) >= 3 && string(out[len(out)-3:]) == "sha" {
			continue
		}
		out = append(out, ch)
	}
	return string(out)
}

// bytesOf reads what script passed as data: bytes as bytes, anything else as
// the text of it.
func (c *cryptoHost) bytesOf(v quickjs.Value) []byte {
	if b, ok := v.Bytes(); ok {
		return b
	}
	return []byte(v.String())
}

// createHash hands back an object holding a running hash. The state lives in
// the closure, so it is collected with the object and cannot be reached except
// through the two methods that are allowed to touch it.
func (c *cryptoHost) createHash(name string) (quickjs.Value, error) {
	h, err := newHash(name)
	if err != nil {
		return quickjs.Value{}, c.rt.Throw(c.rt.NewError("Error", err.Error()))
	}
	return c.hashObject(h)
}

func (c *cryptoHost) createHmac(name string, key quickjs.Value) (quickjs.Value, error) {
	make, err := hashFactory(name)
	if err != nil {
		return quickjs.Value{}, c.rt.Throw(c.rt.NewError("Error", err.Error()))
	}
	return c.hashObject(hmac.New(make, c.bytesOf(key)))
}

// hashFactory is newHash as the constructor hmac and the derivations want.
func hashFactory(name string) (func() hash.Hash, error) {
	if _, err := newHash(name); err != nil {
		return nil, err
	}
	return func() hash.Hash {
		h, _ := newHash(name)
		return h
	}, nil
}

func (c *cryptoHost) hashObject(h hash.Hash) (quickjs.Value, error) {
	done := false
	o := c.rt.NewObject()
	err := setAll(o, map[string]any{
		"update": func(data quickjs.Value) error {
			if done {
				return c.rt.Throw(c.rt.NewError("Error",
					"this hash has already produced its digest"))
			}
			h.Write(c.bytesOf(data))
			return nil
		},
		"digest": func() quickjs.Value {
			done = true
			return c.rt.NewBytes(h.Sum(nil))
		},
	})
	if err != nil {
		return quickjs.Value{}, err
	}
	return o, nil
}

func (c *cryptoHost) randomBytes(n int) (quickjs.Value, error) {
	if n < 0 || n > 1<<24 {
		return quickjs.Value{}, c.rt.Throw(c.rt.NewError("RangeError",
			"the size must be between 0 and 16777216"))
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(c.random, b); err != nil {
		return quickjs.Value{}, err
	}
	return c.rt.NewBytes(b), nil
}

// randomInt is uniform over [min, max), which is what makes it worth asking a
// host for rather than scaling a float.
func (c *cryptoHost) randomInt(min, max int64) (int64, error) {
	if max <= min {
		return 0, c.rt.Throw(c.rt.NewError("RangeError",
			"the upper bound must be above the lower one"))
	}
	n, err := rand.Int(c.random, big.NewInt(max-min))
	if err != nil {
		return 0, err
	}
	return min + n.Int64(), nil
}

// timingSafeEqual compares in a time that does not depend on where the first
// difference is, which is the whole point of comparing a signature this way.
func (c *cryptoHost) timingSafeEqual(a, b quickjs.Value) bool {
	return subtle.ConstantTimeCompare(c.bytesOf(a), c.bytesOf(b)) == 1
}

func (c *cryptoHost) pbkdf2(password, salt quickjs.Value, iterations, length int, name string) (quickjs.Value, error) {
	make, err := hashFactory(name)
	if err != nil {
		return quickjs.Value{}, c.rt.Throw(c.rt.NewError("Error", err.Error()))
	}
	if iterations < 1 || length < 0 || length > 1<<20 {
		return quickjs.Value{}, c.rt.Throw(c.rt.NewError("RangeError",
			"the iteration count and length must be positive"))
	}
	key, err := pbkdf2.Key(make, string(c.bytesOf(password)), c.bytesOf(salt), iterations, length)
	if err != nil {
		return quickjs.Value{}, err
	}
	return c.rt.NewBytes(key), nil
}

func (c *cryptoHost) hkdf(name string, key, salt, info quickjs.Value, length int) (quickjs.Value, error) {
	make, err := hashFactory(name)
	if err != nil {
		return quickjs.Value{}, c.rt.Throw(c.rt.NewError("Error", err.Error()))
	}
	out, err := hkdf.Key(make, c.bytesOf(key), c.bytesOf(salt), string(c.bytesOf(info)), length)
	if err != nil {
		return quickjs.Value{}, c.rt.Throw(c.rt.NewError("Error", err.Error()))
	}
	return c.rt.NewBytes(out), nil
}

// cryptoJS is the shape a program written for node expects around them.
const cryptoJS = `(function (host) {
  "use strict";

  const HEX = "0123456789abcdef";

  function encode(bytes, encoding) {
    switch (String(encoding).toLowerCase()) {
      case "hex": {
        let out = "";
        for (const b of bytes) out += HEX[b >> 4] + HEX[b & 15];
        return out;
      }
      case "base64": case "base64url": {
        let s = "";
        for (const b of bytes) s += String.fromCharCode(b);
        const out = btoa(s);
        if (encoding.toLowerCase() === "base64url") {
          return out.replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
        }
        return out;
      }
      case "latin1": case "binary": {
        let out = "";
        for (const b of bytes) out += String.fromCharCode(b);
        return out;
      }
      case "utf8": case "utf-8": return new TextDecoder().decode(bytes);
    }
    throw new TypeError("unsupported encoding: " + encoding);
  }

  // A digest is bytes; it becomes a Buffer when there is one, since that is
  // what the code being run expects to be handed.
  const wrap = (bytes) => typeof Buffer !== "undefined" ? Buffer.from(bytes) : bytes;

  // Hash and Hmac are the same face: something you feed, and then ask once.
  function faceOf(state) {
    return {
      update(data) { state.update(data); return this; },
      digest(encoding) {
        const bytes = state.digest();
        return encoding === undefined ? wrap(bytes) : encode(bytes, encoding);
      },
    };
  }

  const createHash = (algorithm) => faceOf(host.createHash(String(algorithm)));
  const createHmac = (algorithm, key) => faceOf(host.createHmac(String(algorithm), key));

  function randomBytes(size, callback) {
    if (typeof callback === "function") {
      // The callback form is asynchronous in node, and code that uses it can
      // depend on that: the work itself is not slow enough to need it.
      queueMicrotask(() => {
        try { callback(null, wrap(host.randomBytes(size))); }
        catch (e) { callback(e); }
      });
      return;
    }
    return wrap(host.randomBytes(size));
  }

  function randomFillSync(view, offset = 0, size = view.length - offset) {
    const bytes = host.randomBytes(size);
    new Uint8Array(view.buffer, view.byteOffset + offset, size).set(bytes);
    return view;
  }

  function randomInt(min, max, callback) {
    if (typeof max === "function") { callback = max; max = undefined; }
    if (max === undefined) { max = min; min = 0; }
    if (typeof callback === "function") {
      queueMicrotask(() => {
        try { callback(null, host.randomInt(min, max)); } catch (e) { callback(e); }
      });
      return;
    }
    return host.randomInt(min, max);
  }

  const pbkdf2Sync = (password, salt, iterations, keylen, digest = "sha1") =>
    wrap(host.pbkdf2(password, salt, iterations, keylen, String(digest)));

  const pbkdf2 = (password, salt, iterations, keylen, digest, callback) => {
    if (typeof digest === "function") { callback = digest; digest = "sha1"; }
    // Deriving a key is meant to be slow, so it is handed back through the
    // callback rather than pretending it was free.
    queueMicrotask(() => {
      try { callback(null, pbkdf2Sync(password, salt, iterations, keylen, digest)); }
      catch (e) { callback(e); }
    });
  };

  const hkdfSync = (digest, key, salt, info, keylen) =>
    wrap(host.hkdf(String(digest), key, salt, info, keylen));

  const hkdf = (digest, key, salt, info, keylen, callback) => {
    queueMicrotask(() => {
      try { callback(null, hkdfSync(digest, key, salt, info, keylen).buffer); }
      catch (e) { callback(e); }
    });
  };

  function timingSafeEqual(a, b) {
    if (a.length !== b.length) {
      throw new RangeError("the two arguments must be the same length");
    }
    return host.timingSafeEqual(a, b);
  }

  // The web's crypto.subtle asks for the same primitives under different
  // names. Only digest is there without this module, since only digest can be
  // had without a key; what follows is the rest of what a program signing a
  // webhook or a token needs.
  const raw = new WeakMap();

  class CryptoKey {
    constructor(algorithm, extractable, usages) {
      this.type = "secret";
      this.algorithm = algorithm;
      this.extractable = extractable;
      this.usages = usages;
    }
  }

  const hashOf = (algorithm) => {
    const h = typeof algorithm === "string" ? algorithm : (algorithm && algorithm.hash);
    const name = typeof h === "string" ? h : (h && h.name);
    if (!name) throw new TypeError("the algorithm must say which hash to use");
    return name;
  };
  const named = (algorithm) =>
    String(typeof algorithm === "string" ? algorithm : algorithm.name).toUpperCase();
  const keyBytes = (key) => {
    const bytes = raw.get(key);
    if (!bytes) throw new TypeError("that is not a key this runtime made");
    return bytes;
  };
  const view = (data) => data instanceof ArrayBuffer ? new Uint8Array(data) : data;

  const subtleExtras = {
    async importKey(format, data, algorithm, extractable, usages) {
      if (String(format).toLowerCase() !== "raw") {
        throw new TypeError("only raw keys can be imported here: " + format);
      }
      const name = named(algorithm);
      if (name !== "HMAC" && name !== "PBKDF2" && name !== "HKDF") {
        throw new TypeError("unsupported key algorithm: " + name);
      }
      const key = new CryptoKey(
        name === "HMAC" ? {name, hash: {name: hashOf(algorithm)}} : {name},
        !!extractable, usages || []);
      raw.set(key, new Uint8Array(view(data)));
      return key;
    },
    async exportKey(format, key) {
      if (String(format).toLowerCase() !== "raw") {
        throw new TypeError("only raw keys can be exported here: " + format);
      }
      if (!key.extractable) throw new TypeError("this key may not be exported");
      return keyBytes(key).slice().buffer;
    },
    async sign(algorithm, key, data) {
      if (named(algorithm) !== "HMAC") {
        throw new TypeError("unsupported signing algorithm: " + named(algorithm));
      }
      const state = host.createHmac(String(hashOf(key.algorithm)), keyBytes(key));
      state.update(view(data));
      return state.digest().buffer;
    },
    async verify(algorithm, key, signature, data) {
      const expected = new Uint8Array(await subtleExtras.sign(algorithm, key, data));
      const given = new Uint8Array(view(signature));
      if (given.length !== expected.length) return false;
      return host.timingSafeEqual(given, expected);
    },
    async deriveBits(algorithm, key, length) {
      const bytes = Math.ceil(length / 8);
      const name = named(algorithm);
      if (name === "PBKDF2") {
        return host.pbkdf2(keyBytes(key), view(algorithm.salt),
          algorithm.iterations, bytes, String(hashOf(algorithm))).buffer;
      }
      if (name === "HKDF") {
        return host.hkdf(String(hashOf(algorithm)), keyBytes(key),
          view(algorithm.salt), view(algorithm.info || new Uint8Array(0)), bytes).buffer;
      }
      throw new TypeError("unsupported derivation: " + name);
    },
    async deriveKey(algorithm, baseKey, derived, extractable, usages) {
      const length = derived.length || 256;
      const bits = await subtleExtras.deriveBits(algorithm, baseKey, length);
      return subtleExtras.importKey("raw", bits, derived, extractable, usages);
    },
  };

  if (globalThis.crypto && globalThis.crypto.subtle) {
    for (const [name, fn] of Object.entries(subtleExtras)) {
      Object.defineProperty(globalThis.crypto.subtle, name, {
        value: fn, writable: true, configurable: true,
      });
    }
    if (typeof globalThis.CryptoKey === "undefined") {
      Object.defineProperty(globalThis, "CryptoKey", {
        value: CryptoKey, writable: true, configurable: true,
      });
    }
  }

  const api = {
    createHash, createHmac,
    randomBytes, randomFillSync, randomInt,
    randomUUID: (...args) => globalThis.crypto.randomUUID(...args),
    getRandomValues: (view) => globalThis.crypto.getRandomValues(view),
    timingSafeEqual,
    pbkdf2, pbkdf2Sync, hkdf, hkdfSync,
    getHashes: () => host.hashes.slice(),
    constants: {},
    webcrypto: globalThis.crypto,
    subtle: globalThis.crypto && globalThis.crypto.subtle,
  };
  api.default = api;
  return api;
})`
