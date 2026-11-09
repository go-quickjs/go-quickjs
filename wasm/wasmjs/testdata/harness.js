// A small stand-in for WPT's testharness.js, enough for the WebAssembly
// JavaScript API tests: test, promise_test, setup and the assertions they
// use. Results are collected for the Go test to read with __wptReport().
(function (global) {
  "use strict";
  const results = [];
  const promiseTests = [];
  let setupError = null;
  let unnamed = 0;

  class AssertionError extends Error {}
  AssertionError.prototype.name = "AssertionError";

  function fail(msg) {
    throw new AssertionError(msg);
  }

  function formatValue(v) {
    switch (typeof v) {
    case "string":
      return '"' + v.replace(/\\/g, "\\\\").replace(/"/g, '\\"').replace(/\n/g, "\\n")
        .replace(/[\u0000-\u001f\ud800-\udfff]/g, c => "\\u" + c.charCodeAt(0).toString(16).padStart(4, "0")) + '"';
    case "number":
      return Object.is(v, -0) ? "-0" : String(v);
    case "bigint":
      return String(v) + "n";
    case "symbol":
      return v.toString();
    case "undefined":
    case "boolean":
      return String(v);
    case "function":
      return "function \"" + (v.name || "") + "\"";
    }
    if (v === null) return "null";
    try {
      return "object \"" + String(v) + "\"";
    } catch (e) {
      return "[object]";
    }
  }

  function same(a, b) {
    return Object.is(a, b);
  }

  function prefix(desc) {
    return desc === undefined ? "" : desc + ": ";
  }

  const api = {
    format_value: formatValue,
    assert_equals(actual, expected, desc) {
      if (!same(actual, expected)) {
        fail(`assert_equals: ${prefix(desc)}expected ${formatValue(expected)} but got ${formatValue(actual)}`);
      }
    },
    assert_not_equals(actual, expected, desc) {
      if (same(actual, expected)) {
        fail(`assert_not_equals: ${prefix(desc)}got disallowed value ${formatValue(actual)}`);
      }
    },
    assert_true(v, desc) {
      if (v !== true) fail(`assert_true: ${prefix(desc)}expected true got ${formatValue(v)}`);
    },
    assert_false(v, desc) {
      if (v !== false) fail(`assert_false: ${prefix(desc)}expected false got ${formatValue(v)}`);
    },
    assert_less_than(a, b, desc) {
      if (!(a < b)) fail(`assert_less_than: ${prefix(desc)}expected ${formatValue(a)} < ${formatValue(b)}`);
    },
    assert_greater_than(a, b, desc) {
      if (!(a > b)) fail(`assert_greater_than: ${prefix(desc)}expected ${formatValue(a)} > ${formatValue(b)}`);
    },
    assert_array_equals(actual, expected, desc) {
      if (typeof actual !== "object" || actual === null || !("length" in actual)) {
        fail(`assert_array_equals: ${prefix(desc)}value is ${formatValue(actual)}, expected array`);
      }
      if (actual.length !== expected.length) {
        fail(`assert_array_equals: ${prefix(desc)}lengths differ, expected ${expected.length} got ${actual.length}`);
      }
      for (let i = 0; i < actual.length; i++) {
        if (actual.hasOwnProperty(i) !== expected.hasOwnProperty(i)) {
          fail(`assert_array_equals: ${prefix(desc)}property ${i}, presence differs`);
        }
        if (!same(actual[i], expected[i])) {
          fail(`assert_array_equals: ${prefix(desc)}expected property ${i} to be ${formatValue(expected[i])} but got ${formatValue(actual[i])}`);
        }
      }
    },
    assert_class_string(obj, cls, desc) {
      const got = Object.prototype.toString.call(obj);
      const want = "[object " + cls + "]";
      if (got !== want) fail(`assert_class_string: ${prefix(desc)}expected ${want} but got ${got}`);
    },
    assert_own_property(obj, name, desc) {
      if (!Object.prototype.hasOwnProperty.call(obj, name)) {
        fail(`assert_own_property: ${prefix(desc)}expected property ${formatValue(name)} missing`);
      }
    },
    assert_not_own_property(obj, name, desc) {
      if (Object.prototype.hasOwnProperty.call(obj, name)) {
        fail(`assert_not_own_property: ${prefix(desc)}unexpected property ${formatValue(name)} is found on object`);
      }
    },
    assert_unreached(desc) {
      fail(`assert_unreached: ${prefix(desc)}Reached unreachable code`);
    },
    assert_throws_js(ctor, fn, desc) {
      try {
        fn();
      } catch (e) {
        checkThrown(ctor, e, desc, "assert_throws_js");
        return;
      }
      fail(`assert_throws_js: ${prefix(desc)}${String(fn)} did not throw`);
    },
    assert_throws_exactly(value, fn, desc) {
      try {
        fn();
      } catch (e) {
        if (!same(e, value)) {
          fail(`assert_throws_exactly: ${prefix(desc)}${String(fn)} threw ${formatValue(e)} but we expected it to throw ${formatValue(value)}`);
        }
        return;
      }
      fail(`assert_throws_exactly: ${prefix(desc)}${String(fn)} did not throw`);
    },
    // The legacy form: an error object whose name the thrown one must have.
    assert_throws(code, fn, desc) {
      try {
        fn();
      } catch (e) {
        checkLegacy(code, e, desc, "assert_throws");
        return;
      }
      fail(`assert_throws: ${prefix(desc)}${String(fn)} did not throw`);
    },
    promise_rejects_js(t, ctor, promise, desc) {
      return promise.then(
        () => fail(`promise_rejects_js: ${prefix(desc)}promise was fulfilled`),
        e => checkThrown(ctor, e, desc, "promise_rejects_js"));
    },
    promise_rejects(t, code, promise, desc) {
      return promise.then(
        () => fail(`promise_rejects: ${prefix(desc)}promise was fulfilled`),
        e => checkLegacy(code, e, desc, "promise_rejects"));
    },
    promise_rejects_exactly(t, value, promise, desc) {
      return promise.then(
        () => fail(`promise_rejects_exactly: ${prefix(desc)}promise was fulfilled`),
        e => {
          if (!same(e, value)) fail(`promise_rejects_exactly: ${prefix(desc)}rejected with ${formatValue(e)}`);
        });
    },
    test(fn, name) {
      name = testName(name);
      const t = newTest(name);
      if (setupError !== null) {
        record(name, "FAIL", "setup failed: " + describe(setupError));
        return;
      }
      try {
        fn.call(t, t);
        t.cleanup();
        record(name, "PASS", "");
      } catch (e) {
        t.cleanup();
        record(name, "FAIL", describe(e));
      }
    },
    promise_test(fn, name) {
      promiseTests.push({ fn, name: testName(name) });
    },
    async_test(fn, name) {
      record(testName(name), "FAIL", "async_test is not supported by the harness");
    },
    setup(fn) {
      if (typeof fn !== "function") return;
      try {
        fn();
      } catch (e) {
        setupError = e;
      }
    },
    done() {},
    add_completion_callback() {},
  };

  function checkThrown(ctor, e, desc, what) {
    if (typeof e !== "object" || e === null) {
      fail(`${what}: ${prefix(desc)}threw ${formatValue(e)}, not an object`);
    }
    if (e.constructor !== ctor) {
      fail(`${what}: ${prefix(desc)}threw ${describe(e)} (constructor ${e.constructor && e.constructor.name}) but we expected it to throw ${ctor.name}`);
    }
  }

  function checkLegacy(code, e, desc, what) {
    if (typeof code === "object" && code !== null && "name" in code) {
      if (typeof e !== "object" || e === null || e.name !== code.name) {
        fail(`${what}: ${prefix(desc)}threw ${describe(e)} but we expected it to throw ${code.name}`);
      }
    }
  }

  function describe(e) {
    if (typeof e === "object" && e !== null && "message" in e) {
      return (e.name || "Error") + ": " + e.message;
    }
    return formatValue(e);
  }

  function testName(name) {
    if (typeof name === "string") return name;
    return "unnamed test " + ++unnamed;
  }

  function newTest(name) {
    const cleanups = [];
    return {
      name,
      step(f, self, ...args) { return f.apply(self, args); },
      step_func(f, self) { return function (...args) { return f.apply(self === undefined ? this : self, args); }; },
      step_func_done(f, self) { return function (...args) { if (f) return f.apply(self === undefined ? this : self, args); }; },
      unreached_func(desc) { return function () { fail(`unreached_func: ${prefix(desc)}reached unreachable code`); }; },
      add_cleanup(f) { cleanups.push(f); },
      done() {},
      cleanup() {
        for (const f of cleanups) {
          try { f(); } catch (e) {}
        }
        cleanups.length = 0;
      },
    };
  }

  function record(name, status, message) {
    results.push({ name, status, message });
  }

  // __wptRun runs the promise tests, one after another, and returns a
  // promise that settles when they have run.
  global.__wptRun = async function () {
    for (const { fn, name } of promiseTests) {
      if (setupError !== null) {
        record(name, "FAIL", "setup failed: " + describe(setupError));
        continue;
      }
      const t = newTest(name);
      try {
        await fn.call(t, t);
        record(name, "PASS", "");
      } catch (e) {
        record(name, "FAIL", describe(e));
      } finally {
        t.cleanup();
      }
    }
    return true;
  };
  global.__wptReport = function () {
    return JSON.stringify(results);
  };
  for (const k of Object.keys(api)) {
    Object.defineProperty(global, k, { value: api[k], writable: true, configurable: true });
  }
})(globalThis);
