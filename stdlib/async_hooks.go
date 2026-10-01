package stdlib

import (
	quickjs "github.com/go-quickjs/go-quickjs"
)

// asyncHooks installs node:async_hooks: AsyncLocalStorage and AsyncResource,
// over the runtime's async context, which the engine carries through every
// promise reaction and job, and the loop through every timer.
//
// The context is a map from each storage to its store, never changed once it
// is the context: a run makes a new one, as node's AsyncContextFrame does. Of
// the rest of async_hooks, createHook and the ids are there for code that
// reaches for them, and do nothing -- as in Deno, Bun and Cloudflare's
// workers, where AsyncLocalStorage is what code uses.
func asyncHooks(rt *quickjs.Runtime) error {
	host := rt.NewObject()
	if err := host.Set("get", func(r *quickjs.Runtime) quickjs.Value { return r.AsyncContext() }); err != nil {
		return err
	}
	if err := host.Set("set", func(r *quickjs.Runtime, v quickjs.Value) { r.SetAsyncContext(v) }); err != nil {
		return err
	}
	api, err := evalWithHost(rt, "<async_hooks>", asyncHooksJS, host)
	if err != nil {
		return err
	}
	exports, err := moduleExports(api)
	if err != nil {
		return err
	}
	if err := rt.SetModuleValues("async_hooks", exports); err != nil {
		return err
	}
	return rt.SetModuleValues("node:async_hooks", exports)
}

// asyncHooksJS is node:async_hooks, over the host's async context.
const asyncHooksJS = `(function (host) {
  "use strict";
  const current = () => {
    const ctx = host.get();
    return ctx instanceof Map ? ctx : undefined;
  };
  // with returns a context like ctx but with storage's store set, or
  // removed when there is none.
  const withStore = (ctx, storage, has, store) => {
    const next = new Map(ctx);
    if (has) next.set(storage, store); else next.delete(storage);
    return next;
  };
  const runIn = (ctx, fn, thisArg, args) => {
    const prev = host.get();
    host.set(ctx);
    try {
      return Reflect.apply(fn, thisArg, args);
    } finally {
      host.set(prev);
    }
  };

  class AsyncLocalStorage {
    #enabled = true;
    getStore() {
      if (!this.#enabled) return undefined;
      const ctx = current();
      return ctx === undefined ? undefined : ctx.get(this);
    }
    run(store, fn, ...args) {
      this.#enabled = true;
      return runIn(withStore(current(), this, true, store), fn, undefined, args);
    }
    exit(fn, ...args) {
      return runIn(withStore(current(), this, false), fn, undefined, args);
    }
    enterWith(store) {
      this.#enabled = true;
      host.set(withStore(current(), this, true, store));
    }
    disable() {
      this.#enabled = false;
      const ctx = current();
      if (ctx !== undefined && ctx.has(this)) host.set(withStore(ctx, this, false));
    }
    static bind(fn) {
      return AsyncResource.bind(fn);
    }
    static snapshot() {
      const ctx = host.get();
      return function runInAsyncScope(fn, ...args) {
        return runIn(ctx, fn, this, args);
      };
    }
  }

  class AsyncResource {
    #ctx;
    constructor(type) {
      if (typeof type !== "string" || type === "") {
        const e = new TypeError('The "type" argument must be of type string.');
        e.code = "ERR_INVALID_ARG_TYPE";
        throw e;
      }
      this.type = type;
      this.#ctx = host.get();
    }
    runInAsyncScope(fn, thisArg, ...args) {
      return runIn(this.#ctx, fn, thisArg, args);
    }
    bind(fn, thisArg) {
      const resource = this;
      const bound = function (...args) {
        return resource.runInAsyncScope(fn, thisArg === undefined ? this : thisArg, ...args);
      };
      Object.defineProperty(bound, "length", {value: fn.length, configurable: true});
      return bound;
    }
    static bind(fn, type, thisArg) {
      return new AsyncResource(type || fn.name || "bound-anonymous-fn").bind(fn, thisArg);
    }
    emitDestroy() { return this; }
    asyncId() { return 0; }
    triggerAsyncId() { return 0; }
  }

  const hook = {enable() { return this; }, disable() { return this; }};
  return {
    AsyncLocalStorage,
    AsyncResource,
    createHook: () => hook,
    executionAsyncId: () => 0,
    triggerAsyncId: () => 0,
    executionAsyncResource: () => ({}),
  };
})`
