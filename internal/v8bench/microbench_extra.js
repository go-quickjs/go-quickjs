/*
 * Micro-benchmarks beyond QuickJS's tests/microbench.js, written as its
 * are: each test is a function of n, the work to do, that returns how many
 * operations it did. A test of async functions returns a promise of that
 * count, which the driver awaits; its time includes running the jobs the
 * test queued.
 *
 * Copyright (c) 2026 the go-quickjs authors. Released under the MIT license,
 * as microbench.js is.
 */

function* extra_count_gen(n)
{
    for (var i = 0; i < n; i++)
        yield i;
}

/* a generator iterated with for-of, a yield each turn */
function generator_for_of(n)
{
    var j, v, sum = 0;
    for(j = 0; j < n; j++) {
        for (v of extra_count_gen(100))
            sum += v;
    }
    global_res = sum;
    return n * 100;
}

/* a generator stepped by calling next */
function generator_next(n)
{
    var j, g, r, sum = 0;
    for(j = 0; j < n; j++) {
        g = extra_count_gen(100);
        for (r = g.next(); !r.done; r = g.next())
            sum += r.value;
    }
    global_res = sum;
    return n * 100;
}

/* a generator doing work between yields, which its body's loop runs */
function* extra_busy_gen(n)
{
    var i, s = 0;
    for (i = 0; i < n; i++) {
        s = (s + i * 3) | 0;
        if ((i & 255) == 0)
            yield s;
    }
    return s;
}

function generator_busy(n)
{
    var v, sum = 0;
    for (v of extra_busy_gen(n * 1000))
        sum ^= v;
    global_res = sum;
    return n * 1000;
}

function* extra_pair_gen(a, b)
{
    yield a;
    yield b;
}

/* short generators: making one and running it to the end */
function generator_create(n)
{
    var j, v, sum = 0;
    for(j = 0; j < n; j++) {
        for (v of extra_pair_gen(j, 1))
            sum += v;
    }
    global_res = sum;
    return n;
}

function* extra_delegate_gen(n)
{
    yield* extra_count_gen(n);
}

/* yield* through to another generator */
function generator_delegate(n)
{
    var j, v, sum = 0;
    for(j = 0; j < n; j++) {
        for (v of extra_delegate_gen(100))
            sum += v;
    }
    global_res = sum;
    return n * 100;
}

function* extra_list_gen(head)
{
    for (var node = head; node !== null; node = node.next)
        yield node;
}

/* a generator yielding the objects it walks, a linked list's nodes */
function generator_objects(n)
{
    var j, v, sum = 0, head = null;
    for(j = 0; j < 100; j++)
        head = { value: j, next: head };
    for(j = 0; j < n; j++) {
        for (v of extra_list_gen(head))
            sum += v.value;
    }
    global_res = sum;
    return n * 100;
}

async function extra_await_each(n)
{
    var i, s = 0;
    for (i = 0; i < n; i++)
        s += await i;
    return s;
}

/* an await each turn of a loop */
async function async_await(n)
{
    global_res = await extra_await_each(n * 100);
    return n * 100;
}

async function extra_busy_async(n)
{
    var i, s = 0;
    for (i = 0; i < n; i++) {
        s = (s + i * 3) | 0;
        if ((i & 255) == 0)
            await null;
    }
    return s;
}

/* an async function doing work between awaits, which its body's loop runs */
async function async_busy(n)
{
    global_res = await extra_busy_async(n * 1000);
    return n * 1000;
}

async function extra_add_one(x)
{
    return x + 1;
}

/* calls of an async function that does not await, each awaited */
async function async_call(n)
{
    var j, sum = 0;
    for(j = 0; j < n; j++)
        sum += await extra_add_one(j);
    global_res = sum;
    return n;
}

async function extra_leaf(x)
{
    return x * 2;
}

async function extra_mid(x)
{
    return (await extra_leaf(x)) + 1;
}

async function extra_top(x)
{
    return (await extra_mid(x)) + 1;
}

/* async functions awaiting async functions, three deep */
async function async_chain(n)
{
    var j, sum = 0;
    for(j = 0; j < n; j++)
        sum += await extra_top(j);
    global_res = sum;
    return n;
}

async function* extra_async_count(n)
{
    for (var i = 0; i < n; i++)
        yield i;
}

/* an async generator iterated with for await */
async function async_generator(n)
{
    var j, v, sum = 0;
    for(j = 0; j < n; j++) {
        for await (v of extra_async_count(100))
            sum += v;
    }
    global_res = sum;
    return n * 100;
}

/* a try statement around a loop's body, which never throws */
function try_catch(n)
{
    var i, s = 0;
    for (i = 0; i < n; i++) {
        try {
            s = (s + i) | 0;
        } catch (e) {
            s = 0;
        }
    }
    global_res = s;
    return n;
}

/* an exception thrown and caught each turn */
function try_throw(n)
{
    var i, s = 0;
    for (i = 0; i < n; i++) {
        try {
            throw i;
        } catch (e) {
            s = (s + e) | 0;
        }
    }
    global_res = s;
    return n;
}

/* a finally clause run each turn */
function try_finally(n)
{
    var i, s = 0;
    for (i = 0; i < n; i++) {
        try {
            s = (s + i) | 0;
        } finally {
            s ^= 1;
        }
    }
    global_res = s;
    return n;
}

function ExtraRange(n)
{
    this.i = 0;
    this.n = n;
}
ExtraRange.prototype.next = function () {
    if (this.i < this.n)
        return { value: this.i++, done: false };
    return { value: undefined, done: true };
};
ExtraRange.prototype[Symbol.iterator] = function () { return this; };

/* a for-of loop over an iterator written in JavaScript */
function iterator_for_of(n)
{
    var j, v, sum = 0;
    for(j = 0; j < n; j++) {
        for (v of new ExtraRange(100))
            sum += v;
    }
    global_res = sum;
    return n * 100;
}

/* a for-of loop over a Map's values */
function map_for_of(n)
{
    var i, j, v, sum = 0, m = new Map();
    for (i = 0; i < 100; i++)
        m.set(i, i);
    for(j = 0; j < n; j++) {
        for (v of m.values())
            sum += v;
    }
    global_res = sum;
    return n * 100;
}

/* a for-in loop over objects of one layout, the keys of the layout */
function object_for_in(n)
{
    var j, k, c = 0, objs = [];
    for (j = 0; j < 4; j++)
        objs.push({ a: j, b: 2, c: 3, d: 4, e: 5, f: 6, g: 7, h: 8 });
    for (j = 0; j < n; j++) {
        for (k in objs[j & 3])
            c++;
    }
    global_res = c;
    return n * 8;
}

/* fixed data properties, all filled before the object leaves the literal */
function object_literal_fixed(n)
{
    var j, o;
    for (j = 0; j < n; j++)
        o = { a: j, b: j + 1, c: "key", d: true,
              e: null, f: j, g: false, h: "end" };
    global_res = o;
    return n;
}

/* repeated growth past the small array capacity classes */
function array_push_large(n)
{
    var i, j, a;
    for (j = 0; j < n; j++) {
        a = [];
        for (i = 0; i < 8192; i++)
            a.push(i);
    }
    global_res = a.length + a[8191];
    return n * 8192;
}

/* The same fragment groups appended by one call or by single-argument calls.
   An operation is one group, three or five elements, including array growth. */
function array_push_args3(n)
{
    var i, j, a;
    for (j = 0; j < n; j++) {
        a = [];
        for (i = 0; i < 1024; i++)
            a.push(" ", "disabled", '=""');
    }
    global_res = a;
    return n * 1024;
}

function array_push_args3_single(n)
{
    var i, j, a;
    for (j = 0; j < n; j++) {
        a = [];
        for (i = 0; i < 1024; i++) {
            a.push(" ");
            a.push("disabled");
            a.push('=""');
        }
    }
    global_res = a;
    return n * 1024;
}

function array_push_args5(n)
{
    var i, j, a;
    for (j = 0; j < n; j++) {
        a = [];
        for (i = 0; i < 1024; i++)
            a.push(" ", "data-id", '="', "42", '"');
    }
    global_res = a;
    return n * 1024;
}

function array_push_args5_single(n)
{
    var i, j, a;
    for (j = 0; j < n; j++) {
        a = [];
        for (i = 0; i < 1024; i++) {
            a.push(" ");
            a.push("data-id");
            a.push('="');
            a.push("42");
            a.push('"');
        }
    }
    global_res = a;
    return n * 1024;
}

/* Borrowed hasOwnProperty.call on cached string keys, and a direct-method
   control. False-valued own properties count; inherited properties do not. */
var extra_own_object = { className: 0, title: false, id: null, "data-kind": true };
var extra_own_hit_keys = ["className", "title", "id", "data-kind"];
var extra_own_miss_keys = ["missing", "toString", "valueOf", "constructor"];

function has_own_call_hit(n)
{
    var j, s = 0, own = Object.prototype.hasOwnProperty;
    for (j = 0; j < n; j++)
        if (own.call(extra_own_object, extra_own_hit_keys[j & 3]))
            s++;
    global_res = s;
    return n;
}

function has_own_call_miss(n)
{
    var j, s = 0, own = Object.prototype.hasOwnProperty;
    for (j = 0; j < n; j++)
        if (own.call(extra_own_object, extra_own_miss_keys[j & 3]))
            s++;
    global_res = s;
    return n;
}

function has_own_call_for_in(n)
{
    var j, k, s = 0, own = Object.prototype.hasOwnProperty;
    for (j = 0; j < n; j++) {
        for (k in extra_own_object)
            if (own.call(extra_own_object, k))
                s++;
    }
    global_res = s;
    return n * 4;
}

function has_own_method_hit(n)
{
    var j, s = 0;
    for (j = 0; j < n; j++)
        if (extra_own_object.hasOwnProperty(extra_own_hit_keys[j & 3]))
            s++;
    global_res = s;
    return n;
}

/* a callback appending fragments to a captured string, then reading it */
function upvalue_string_append(n)
{
    var i, j, s, sum = 0;
    var sink = { push: function (x) { return x !== null && (s += x), true; } };
    for (j = 0; j < n; j += 1024) {
        s = "";
        for (i = 0; i < 1024; i++)
            sink.push("<x>");
        sum += s.length + s.lastIndexOf("<x>");
    }
    global_res = sum;
    return j;
}

/* growth by half: the length it does worst at against growth by a
   quarter, and a long array. n is pushes, so that a small n is a small
   test, as TestJITSameWorkWhileMarking runs each with the collector off */
function array_push_28k(n)
{
    var i, j, a, len = n < 27668 ? n : 27668;
    for (j = 0; j < n; j += len) {
        a = [];
        for (i = 0; i < len; i++)
            a.push(i);
    }
    global_res = a.length;
    return j;
}

function array_push_1m(n)
{
    var i, j, a, len = n < 1000000 ? n : 1000000;
    for (j = 0; j < n; j += len) {
        a = [];
        for (i = 0; i < len; i++)
            a.push(i);
    }
    global_res = a.length;
    return j;
}

/* An allocation site whose arrays are mostly short, with an occasional
   larger result. A capacity hint must give the spare room back. */
function extra_array_fill(n)
{
    var a = [];
    for (var i = 0; i < n; i++)
        a.push(i);
    return a;
}

function array_push_varying(n)
{
    var j, a, count = 0;
    for (j = 0; j < n; j++) {
        a = extra_array_fill((j & 31) === 0 ? Math.min(n, 32768) : 4);
        count += a.length;
    }
    global_res = a.length + a[0];
    return count;
}

function array_literal_empty(n)
{
    var j, a;
    for (j = 0; j < n; j++)
        a = [];
    global_res = a.length;
    return n;
}

/* repeated growth by an assignment at the array's length */
function array_index_append(n)
{
    var i, j, a, s = 0;
    for (j = 0; j < n; j++) {
        a = [];
        for (i = 0; i < 8192; i++)
            a[a.length] = i;
        s += a.length;
    }
    global_res = s;
    return n * 8192;
}

/* for-in loops that the last loop's cursor cannot serve: left at their
   first key, so none finishes */
function for_in_break(n)
{
    var j, k, c = 0, o = { a: 1, b: 2, c: 3, d: 4 };
    for (j = 0; j < n; j++) {
        for (k in o) {
            c++;
            break;
        }
    }
    global_res = c;
    return n;
}

/* a loop in each key of a loop: the outer's cursor is never the last */
function for_in_nested(n)
{
    var j, k, l, c = 0, o = { a: 1, b: 2, c: 3, d: 4 };
    for (j = 0; j < n; j++) {
        for (k in o) {
            for (l in o)
                c++;
        }
    }
    global_res = c;
    return n * 16;
}

/* what growing a captured string in place does worst: a read after each
   append, a read after each two, an append or two to each new binding, and
   a captured number added to the same way */
function append_read_each(n)
{
    var i, j, s, c = 0;
    var sink = { push: function (x) { s += x; },
                 len: function () { return s.length; } };
    for (j = 0; j < n; j += 1024) {
        s = "";
        for (i = 0; i < 1024; i++) {
            sink.push("<x>");
            c += sink.len();
        }
    }
    global_res = c;
    return j;
}

function append_pair_read(n)
{
    var i, j, s, c = 0;
    var sink = { push: function (x) { s += x; },
                 len: function () { return s.length; } };
    for (j = 0; j < n; j += 2048) {
        s = "";
        for (i = 0; i < 1024; i++) {
            sink.push("<x>");
            sink.push("<y>");
            c += sink.len();
        }
    }
    global_res = c;
    return j;
}

/* the same with a read that keeps the string, as x.length does not */
function append_keep_each(n)
{
    var i, j, s, c = 0;
    var sink = { push: function (x) { s += x; },
                 get: function () { return s; } };
    for (j = 0; j < n; j += 1024) {
        s = "";
        for (i = 0; i < 1024; i++) {
            sink.push("<x>");
            c += sink.get().length;
        }
    }
    global_res = c;
    return j;
}

function append_keep_pair(n)
{
    var i, j, s, c = 0;
    var sink = { push: function (x) { s += x; },
                 get: function () { return s; } };
    for (j = 0; j < n; j += 2048) {
        s = "";
        for (i = 0; i < 1024; i++) {
            sink.push("<x>");
            sink.push("<y>");
            c += sink.get().length;
        }
    }
    global_res = c;
    return j;
}

function extra_append_once(a, x)
{
    var s = a;
    var add = function (y) { s += y; };
    add(x);
    return s;
}

function extra_append_twice(a, x)
{
    var s = a;
    var add = function (y) { s += y; };
    add(x);
    add(x);
    return s;
}

function upvalue_short_append(n)
{
    var j, c = 0, a = "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnop";
    for (j = 0; j < n; j++)
        c += extra_append_once(a, "<x>").length;
    global_res = c;
    return n;
}

function upvalue_two_appends(n)
{
    var j, c = 0, a = "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnop";
    for (j = 0; j < n; j++)
        c += extra_append_twice(a, "<x>").length;
    global_res = c;
    return n;
}

function upvalue_sum(n)
{
    var j, t = 0;
    var add = function (x) { t += x; };
    for (j = 0; j < n; j++)
        add(j & 7);
    global_res = t;
    return n;
}

/* a template's render, as a compiled template is written: a local a string
   is built in, its pieces and what an escape function returned, with no loop
   and in one; numbers among the pieces; and what it does worst, one append
   to a local and a read after each */
function extra_esc(s)
{
    return s.indexOf("<") < 0 ? s : s.replace(/</g, "&lt;");
}

function extra_row(a, b, c)
{
    var out = "<tr><td>";
    out += extra_esc(a);
    out += "</td><td>";
    out += extra_esc(b);
    out += "</td><td>";
    out += extra_esc(c);
    out += "</td></tr>";
    return out;
}

function template_row(n)
{
    var j, c = 0;
    for (j = 0; j < n; j++)
        c += extra_row("alpha", "beta<gamma", "delta").length;
    global_res = c;
    return n;
}

function extra_list(xs)
{
    var i, out = "<ul>";
    for (i = 0; i < xs.length; i++) {
        out += "<li class=\"item\">";
        out += extra_esc(xs[i]);
        out += "</li>";
    }
    out += "</ul>";
    return out;
}

function template_list(n)
{
    var j, c = 0, xs = [];
    for (j = 0; j < 64; j++)
        xs.push("item number " + j);
    for (j = 0; j < n; j += 64)
        c += extra_list(xs).length;
    global_res = c;
    return j;
}

function template_escaped(n)
{
    var j, c = 0, xs = [];
    for (j = 0; j < 64; j++)
        xs.push("item <b>" + j + "</b>");
    for (j = 0; j < n; j += 64)
        c += extra_list(xs).length;
    global_res = c;
    return j;
}

function template_numbers(n)
{
    var i, j, out, c = 0;
    for (j = 0; j < n; j += 64) {
        out = "";
        for (i = 0; i < 64; i++) {
            out += i;
            out += ",";
        }
        c += out.length;
    }
    global_res = c;
    return j;
}

function extra_once(a, x)
{
    var out = "<" + a;
    out += x;
    return out;
}

function local_append_once(n)
{
    var j, c = 0, a = "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnop";
    for (j = 0; j < n; j++)
        c += extra_once(a, "<x>").length;
    global_res = c;
    return n;
}

function local_append_read(n)
{
    var i, j, out, c = 0;
    for (j = 0; j < n; j += 1024) {
        out = "";
        for (i = 0; i < 1024; i++) {
            out += "<x>";
            c += out.length;
        }
    }
    global_res = c;
    return j;
}

function local_append_keep(n)
{
    var i, j, out, held, c = 0;
    for (j = 0; j < n; j += 1024) {
        out = "";
        for (i = 0; i < 1024; i++) {
            out += "<x>";
            held = out;
            c += held.length;
        }
    }
    global_res = c;
    return j;
}

/* a for-in loop over a dictionary: an object a key was deleted from,
   not the last added */
function dict_for_in(n)
{
    var j, k, c = 0, d = { z: 0, a: 1, b: 2, c: 3, d: 4, e: 5, f: 6, g: 7, h: 8 };
    delete d.z;
    for (j = 0; j < n; j++) {
        for (k in d)
            c++;
    }
    global_res = c;
    return n * 8;
}

/* a scope with a using declaration each turn, made with Function, as an
   engine without using declarations cannot parse them: there it does
   nothing, and reports none */
var extra_resource = { count: 0 };
if (typeof Symbol.dispose === "symbol")
    extra_resource[Symbol.dispose] = function () { this.count++; };
var extra_using;

function using_scope(n)
{
    if (extra_using === undefined) {
        try {
            extra_using = new Function("n", "r", "var i, s = 0; for (i = 0; i < n; i++) { using u = r; s = (s + i) | 0; } return s;");
        } catch (e) {
            extra_using = null;
        }
    }
    if (!extra_using)
        return 0;
    global_res = extra_using(n, extra_resource);
    return n;
}

/* a closure made each turn, capturing the turn's binding, and called */
function closure_make(n)
{
    var j, s = 0;
    for (j = 0; j < n; j++) {
        let k = j;
        var f = function () { return k; };
        s = (s + f()) | 0;
    }
    global_res = s;
    return n;
}

/* a local the loop assigns, which a closure reads, called each turn */
function closure_counter(n)
{
    var j, x = 0, s = 0;
    function get() { return x; }
    for (j = 0; j < n; j++) {
        x = j;
        s = (s + get()) | 0;
    }
    global_res = s;
    return n;
}

/* a function that calls its callback, given a new closure at each call */
function closure_each(a, f)
{
    var i, s = 0;
    for (i = 0; i < a.length; i++)
        s = (s + f(a[i])) | 0;
    return s;
}

function closure_callback(n)
{
    var j, s = 0, a = [1, 2, 3, 4, 5, 6, 7, 8];
    for (j = 0; j < n; j++)
        s = (s + closure_each(a, function (x) { return x * j; })) | 0;
    global_res = s;
    return n * 8;
}

/* a function that calls itself in tail position, 1024 deep, past the depth
   where a call in tail position gives up its frame */
function tail_self_loop(n, acc)
{
    "use strict";
    if (n === 0)
        return acc;
    return tail_self_loop(n - 1, (acc + n) | 0);
}

function tail_self(n)
{
    var j, s = 0;
    for (j = 0; j < n; j += 1024)
        s = (s + tail_self_loop(1024, j)) | 0;
    global_res = s;
    return n;
}

/* two functions that call each other in tail position, 1024 deep */
function tail_even(n)
{
    "use strict";
    return n === 0 ? 1 : tail_odd(n - 1);
}

function tail_odd(n)
{
    "use strict";
    return n === 0 ? 0 : tail_even(n - 1);
}

function tail_mutual(n)
{
    var j, s = 0;
    for (j = 0; j < n; j += 1024)
        s = (s + tail_even(1024 + (j & 1))) | 0;
    global_res = s;
    return n;
}

/* calls nothing inlines: a function calling itself twice, fib(10)'s 177
   calls a turn, timed a call */
function call_fib_rec(k)
{
    return k < 2 ? k : call_fib_rec(k - 1) + call_fib_rec(k - 2);
}

function call_fib(n)
{
    var j, s = 0;
    for (j = 0; j < n; j += 177)
        s = (s + call_fib_rec(10)) | 0;
    global_res = s;
    return n;
}

/* two functions that call each other, not in tail position, 64 deep: what
   EarleyBoyer's rewrite_nboyer and rewrite_args_nboyer make, timed a call */
function call_even(k)
{
    return k === 0 ? 1 : call_odd(k - 1) + 1;
}

function call_odd(k)
{
    return k === 0 ? 0 : call_even(k - 1) + 1;
}

function call_mutual(n)
{
    var j, s = 0;
    for (j = 0; j < n; j += 65)
        s = (s + call_even(64)) | 0;
    global_res = s;
    return n;
}

/* a method whose body has a loop, which it does not enter, so that it is
   called with a frame of its own, and a method that only returns its
   argument. The loop makes a call, so that the JIT does not inline the
   method either, as it does one whose loop calls nothing. */
function ExtraPoint(x)
{
    this.x = x;
}

ExtraPoint.prototype.sum = function (k)
{
    var j, s = 0;
    for (j = 0; j < k; j++)
        s += this.same(this.x);
    return s;
};

ExtraPoint.prototype.same = function (k)
{
    return k;
};

function method_call(n)
{
    var j, o = new ExtraPoint(1), s = 0;
    for (j = 0; j < n; j++)
        s += o.sum(0);
    global_res = s;
    return n;
}

function method_call_leaf(n)
{
    var j, o = new ExtraPoint(1), s = 0;
    for (j = 0; j < n; j++)
        s += o.same(j);
    global_res = s;
    return n;
}

/* loops of a few simple instructions each: with QJS_NOTREE=1, what the
   interpreter's dispatch costs */
function loop_sum(n)
{
    var j, s = 0;
    for (j = 0; j < n; j++)
        s += j;
    global_res = s;
    return n;
}

function loop_arith(n)
{
    var j, a = 1, b = 2, c = 0;
    for (j = 0; j < n; j++) {
        c = (a + b) * 3 - c;
        a = b - c;
        b = c & 1023;
    }
    global_res = c;
    return n;
}

function loop_locals(n)
{
    var j, a = 1, b = 2, c = 3, d = 4;
    for (j = 0; j < n; j++) {
        a = b;
        b = c;
        c = d;
        d = a;
    }
    global_res = a + b + c + d;
    return n;
}

function loop_elements(n)
{
    var j, t = [1, 2, 3, 4, 5, 6, 7, 8], s = 0;
    for (j = 0; j < n; j++)
        s += t[j & 7];
    global_res = s;
    return n;
}

function loop_props(n)
{
    var j, o = { x: 1, y: 2 }, s = 0;
    for (j = 0; j < n; j++)
        s += o.x + o.y;
    global_res = s;
    return n;
}

/* Emscripten's integers: each value an int32 by | 0, its counter's
   comparison and its arithmetic the int32s' */
function loop_int32(n)
{
    var j = 0, a = 1, b = 2, s = 0;
    for (j = 0; (j | 0) < (n | 0); j = (j + 1) | 0) {
        a = (a * 1103 + b) | 0;
        b = (b ^ (a >>> 7)) | 0;
        s = (s + (a & 255) - (b >> 29)) | 0;
    }
    global_res = s;
    return n;
}

/* long string switches: early hits, late hits, misses and mixed keys */
function extra_string_switch(n, keys)
{
    var j, sum = 0;
    for (j = 0; j < n; j++) {
        switch (keys[j & 3]) {
        case "a": sum += 1; break;
        case "div": sum += 2; break;
        case "p": sum += 3; break;
        case "span": sum += 4; break;
        case "button": sum += 5; break;
        case "input": sum += 6; break;
        case "form": sum += 7; break;
        case "label": sum += 8; break;
        case "select": sum += 9; break;
        case "option": sum += 10; break;
        case "table": sum += 11; break;
        case "tr": sum += 12; break;
        case "td": sum += 13; break;
        case "ul": sum += 14; break;
        case "li": sum += 15; break;
        case "section": sum += 16; break;
        default: break;
        }
    }
    global_res = sum;
    return n;
}

function string_switch_first(n)
{
    return extra_string_switch(n, ["a", "a", "a", "a"]);
}

function string_switch_last(n)
{
    return extra_string_switch(n, ["section", "section", "section", "section"]);
}

function string_switch_miss(n)
{
    return extra_string_switch(n, ["unknown", "unknown", "unknown", "unknown"]);
}

function string_switch_mixed(n)
{
    return extra_string_switch(n, ["a", "section", "unknown", "label"]);
}

/* the same switch on strings made at run time, as a parser's or a DOM's
   are, rather than the literals the cases are written with */
function string_switch_built(n)
{
    var keys = ["a", "section", "unknown", "label"].map(function (k) {
        return (k + "?").slice(0, k.length);
    });
    return extra_string_switch(n, keys);
}

/* a switch of three cases, as on an HTTP method, one key a miss */
function string_switch_small(n)
{
    var j, sum = 0, keys = ["GET", "POST", "PUT", "DELETE"];
    for (j = 0; j < n; j++) {
        switch (keys[j & 3]) {
        case "GET": sum += 1; break;
        case "POST": sum += 2; break;
        case "PUT": sum += 3; break;
        default: break;
        }
    }
    global_res = sum;
    return n;
}

/* a switch of forty cases, a lexer's keyword table */
function string_switch_large(n)
{
    var j, sum = 0, keys = ["if", "return", "function", "protected"];
    for (j = 0; j < n; j++) {
        switch (keys[j & 3]) {
        case "break": sum += 1; break;
        case "case": sum += 2; break;
        case "catch": sum += 3; break;
        case "class": sum += 4; break;
        case "const": sum += 5; break;
        case "continue": sum += 6; break;
        case "debugger": sum += 7; break;
        case "default": sum += 8; break;
        case "delete": sum += 9; break;
        case "do": sum += 10; break;
        case "else": sum += 11; break;
        case "export": sum += 12; break;
        case "extends": sum += 13; break;
        case "finally": sum += 14; break;
        case "for": sum += 15; break;
        case "function": sum += 16; break;
        case "if": sum += 17; break;
        case "import": sum += 18; break;
        case "in": sum += 19; break;
        case "instanceof": sum += 20; break;
        case "new": sum += 21; break;
        case "return": sum += 22; break;
        case "super": sum += 23; break;
        case "switch": sum += 24; break;
        case "this": sum += 25; break;
        case "throw": sum += 26; break;
        case "try": sum += 27; break;
        case "typeof": sum += 28; break;
        case "var": sum += 29; break;
        case "void": sum += 30; break;
        case "while": sum += 31; break;
        case "with": sum += 32; break;
        case "yield": sum += 33; break;
        case "let": sum += 34; break;
        case "static": sum += 35; break;
        case "enum": sum += 36; break;
        case "await": sum += 37; break;
        case "implements": sum += 38; break;
        case "package": sum += 39; break;
        case "protected": sum += 40; break;
        default: break;
        }
    }
    global_res = sum;
    return n;
}

/* cases that share their first letters, as DOM event names do */
function string_switch_prefix(n)
{
    var j, sum = 0, keys = ["mouseleave", "touchcancel", "pointerup", "mousemove"];
    for (j = 0; j < n; j++) {
        switch (keys[j & 3]) {
        case "mousedown": sum += 1; break;
        case "mouseup": sum += 2; break;
        case "mousemove": sum += 3; break;
        case "mouseover": sum += 4; break;
        case "mouseout": sum += 5; break;
        case "mouseenter": sum += 6; break;
        case "mouseleave": sum += 7; break;
        case "keydown": sum += 8; break;
        case "keyup": sum += 9; break;
        case "keypress": sum += 10; break;
        case "touchstart": sum += 11; break;
        case "touchmove": sum += 12; break;
        case "touchend": sum += 13; break;
        case "touchcancel": sum += 14; break;
        case "pointerdown": sum += 15; break;
        case "pointerup": sum += 16; break;
        default: break;
        }
    }
    global_res = sum;
    return n;
}

/* long cases with long common prefixes, as a reducer's action types are,
   one key a miss that differs only at its end */
function string_switch_long(n)
{
    var j, sum = 0, keys = ["filters/colorFilterChanged", "todos/todosLoaded",
                            "user/loggedOut", "todos/allCompletedX"];
    for (j = 0; j < n; j++) {
        switch (keys[j & 3]) {
        case "todos/todoAdded": sum += 1; break;
        case "todos/todoToggled": sum += 2; break;
        case "todos/todoDeleted": sum += 3; break;
        case "todos/colorSelected": sum += 4; break;
        case "todos/allCompleted": sum += 5; break;
        case "todos/completedCleared": sum += 6; break;
        case "todos/todosLoading": sum += 7; break;
        case "todos/todosLoaded": sum += 8; break;
        case "filters/statusFilterChanged": sum += 9; break;
        case "filters/colorFilterChanged": sum += 10; break;
        case "user/loggedIn": sum += 11; break;
        case "user/loggedOut": sum += 12; break;
        default: break;
        }
    }
    global_res = sum;
    return n;
}

/* a switch on typeof, as a serializer's or a validator's is */
function string_switch_typeof(n)
{
    var j, sum = 0, values = [1, "s", null, undefined];
    for (j = 0; j < n; j++) {
        switch (typeof values[j & 3]) {
        case "number": sum += 1; break;
        case "string": sum += 2; break;
        case "boolean": sum += 3; break;
        case "object": sum += 4; break;
        case "undefined": sum += 5; break;
        case "function": sum += 6; break;
        case "bigint": sum += 7; break;
        case "symbol": sum += 8; break;
        default: break;
        }
    }
    global_res = sum;
    return n;
}

/* the switch of strings given values that are no strings: every case a
   miss, 1 not even "1" */
function string_switch_nonstring(n)
{
    return extra_string_switch(n, [1, null, undefined, 3.5]);
}

/* a switch of the tag names with one case that is no literal, a name
   held in a variable, as a parser's set of tags configured is */
function string_switch_nonliteral(n)
{
    var j, sum = 0, custom = "header",
        keys = ["div", "header", "unknown", "section"];
    for (j = 0; j < n; j++) {
        switch (keys[j & 3]) {
        case "a": sum += 1; break;
        case "div": sum += 2; break;
        case "p": sum += 3; break;
        case "span": sum += 4; break;
        case custom: sum += 5; break;
        case "button": sum += 6; break;
        case "input": sum += 7; break;
        case "form": sum += 8; break;
        case "label": sum += 9; break;
        case "select": sum += 10; break;
        case "option": sum += 11; break;
        case "table": sum += 12; break;
        case "ul": sum += 13; break;
        case "li": sum += 14; break;
        case "section": sum += 15; break;
        default: break;
        }
    }
    global_res = sum;
    return n;
}

/* a switch of sixteen numbers, as an interpreter's on its opcodes or a
   state machine's on its states, one key a miss */
function number_switch(n)
{
    var j, sum = 0, keys = [1, 9, 16, 17];
    for (j = 0; j < n; j++) {
        switch (keys[j & 3]) {
        case 1: sum += 1; break;
        case 2: sum += 2; break;
        case 3: sum += 3; break;
        case 4: sum += 4; break;
        case 5: sum += 5; break;
        case 6: sum += 6; break;
        case 7: sum += 7; break;
        case 8: sum += 8; break;
        case 9: sum += 9; break;
        case 10: sum += 10; break;
        case 11: sum += 11; break;
        case 12: sum += 12; break;
        case 13: sum += 13; break;
        case 14: sum += 14; break;
        case 15: sum += 15; break;
        case 16: sum += 16; break;
        default: break;
        }
    }
    global_res = sum;
    return n;
}

/* typeof v === "function" over values of every kind, as a callback's
   guard or an options check is */
function typeof_test(n)
{
    var j, sum = 0,
        values = [1, "s", function () {}, null, {}, undefined, true, Math.max];
    for (j = 0; j < n; j++) {
        if (typeof values[j & 7] === "function")
            sum += 1;
    }
    global_res = sum;
    return n;
}

/* a class's private field, read and written, and its private method,
   called */
class PrivateCounter
{
    #n = 0;
    #get() { return this.#n; }
    add(n)
    {
        var j;
        for (j = 0; j < n; j++)
            this.#n = (this.#n + j) | 0;
        return this.#n;
    }
    sum(n)
    {
        var j, s = 0;
        for (j = 0; j < n; j++)
            s = (s + this.#get()) | 0;
        return s;
    }
}

function private_field(n)
{
    global_res = new PrivateCounter().add(n);
    return n;
}

function private_method(n)
{
    var c = new PrivateCounter();
    c.add(10);
    global_res = c.sum(n);
    return n;
}

/* Math.random called in a loop, its numbers summed */
function math_random(n)
{
    var j, s = 0;
    for (j = 0; j < n; j++)
        s += Math.random();
    global_res = s;
    return n;
}

/* Math.imul in a hash's loop, FNV-1a's of the counter */
function math_imul(n)
{
    var j, h = 0x811c9dc5 | 0;
    for (j = 0; j < n; j++)
        h = Math.imul(h ^ j, 16777619);
    global_res = h;
    return n;
}

/* a template of three parts and the strings between them */
function string_template(n)
{
    var j, s = "id", r;
    for (j = 0; j < n; j++)
        r = `${s}:${j}-${j & 7}`;
    global_res = r;
    return n;
}

/* a chain of ten parts, calls among them, which the compiler joins in one
   concat */
function string_chain(n)
{
    var j, r;
    for (j = 0; j < n; j++)
        r = "<" + String(j) + ":" + String(j & 7) + ":" + String(j & 3) + ":" + String(j & 1) + ":" + String(j & 15) + ">";
    global_res = r;
    return n;
}

/* Math.clz32 of the counter, summed */
function math_clz32(n)
{
    var j, s = 0;
    for (j = 0; j < n; j++)
        s += Math.clz32(j);
    global_res = s;
    return n;
}

/* Math.pow of a fraction to a fraction, which no multiplication makes */
function math_pow(n)
{
    var j, s = 0;
    for (j = 0; j < n; j++)
        s += Math.pow(1.0001 + (j & 7) * 0.001, 2.5);
    global_res = s;
    return n;
}

/* Math.atan2 and Math.hypot of a point, an angle and a distance */
function math_atan2_hypot(n)
{
    var j, s = 0;
    for (j = 0; j < n; j++)
        s += Math.atan2(j & 15, 7) + Math.hypot(j & 15, 7);
    global_res = s;
    return n;
}

/* an array of 1000 shuffled again and again, as Fisher and Yates do: a
   Math.random and a swap for each element */
function random_shuffle(n)
{
    var a = [], i, j, k, t, r;
    for (i = 0; i < 1000; i++)
        a.push(i);
    for (r = 0; r < n; r += 999) {
        for (j = a.length - 1; j > 0; j--) {
            k = Math.floor(Math.random() * (j + 1));
            t = a[j];
            a[j] = a[k];
            a[k] = t;
        }
    }
    global_res = a[0];
    return r;
}

/* the in operator over an object, its keys constants: an own key, an
   absent one, one inherited and one of Object.prototype */
function prop_in(n)
{
    var proto = { inherited: 1 }, obj = Object.create(proto), j, s = 0;
    obj.a = 1;
    obj.b = 2;
    obj.c = 3;
    for (j = 0; j < n; j++) {
        if ("a" in obj) s++;
        if ("z" in obj) s++;
        if ("inherited" in obj) s++;
        if ("toString" in obj) s++;
    }
    global_res = s;
    return n * 4;
}

/* the in operator over an object, its key a variable's */
function prop_in_keyed(n)
{
    var obj = { a: 1, b: 2, c: 3 }, keys = ["a", "z", "c", "y"], j, s = 0;
    for (j = 0; j < n; j++) {
        if (keys[j & 3] in obj) s++;
    }
    global_res = s;
    return n;
}

/* the in operator over an array's indices: in range, a hole, past its
   length */
function array_in(n)
{
    var a = [1, 2, 3, , 5, 6, 7, 8], j, s = 0;
    for (j = 0; j < n; j++) {
        if ((j & 7) in a) s++;
        if (12 in a) s++;
    }
    global_res = s;
    return n * 2;
}

/* a property added to an object and deleted again, its last: back and
   forth along one transition */
function prop_delete_last(n)
{
    var obj = { a: 1, b: 2 }, j;
    for (j = 0; j < n; j++) {
        obj.c = j;
        delete obj.c;
    }
    global_res = obj.a;
    return n;
}

/* object spread of a plain object of four properties, a field after it:
   the copy, and a field defined past it */
function object_spread(n)
{
    var ref = { a: 1, b: 2, c: 3, d: 4 }, j, o;
    for (j = 0; j < n; j++)
        o = { ...ref, k: j };
    global_res = o;
    return n;
}

/* object spread of an object with a getter among its properties, which
   the copy calls */
function object_getter_spread(n)
{
    var ref = { a: 1, get b() { return this.a + 1; }, c: 3 }, j, o;
    for (j = 0; j < n; j++)
        o = { ...ref, k: j };
    global_res = o;
    return n;
}

/* an object pattern's rest element, of the four properties it does not
   name */
function object_rest(n)
{
    var ref = { a: 1, b: 2, c: 3, d: 4, e: 5 }, j, a, r;
    for (j = 0; j < n; j++) {
        ({ a, ...r } = ref);
    }
    global_res = r;
    return n;
}

/* an object literal of a computed key and a field */
function object_computed(n)
{
    var keys = ["x", "y", "z", "w"], j, o;
    for (j = 0; j < n; j++)
        o = { [keys[j & 3]]: j, v: 1 };
    global_res = o;
    return n;
}

/* an array literal of an element and an array of eight spread */
function array_spread(n)
{
    var ref = [1, 2, 3, 4, 5, 6, 7, 8], j, a;
    for (j = 0; j < n; j++)
        a = [j, ...ref];
    global_res = a;
    return n;
}

function extra_sum3(a, b, c)
{
    return a + b + c;
}

/* a call whose arguments are an array of three spread */
function spread_call(n)
{
    var args = [1, 2, 3], j, s = 0;
    for (j = 0; j < n; j++)
        s += extra_sum3(...args);
    global_res = s;
    return n;
}

/* Array.prototype.forEach over a hundred numbers, its callback an arrow
   made anew at each call that adds to a binding it captured, as AWFY
   NBody's advance has it */
function array_forEach(n)
{
    var r, i, j, sum, len = 100;
    r = [];
    for (i = 0; i < len; i++)
        r[i] = i;
    for (j = 0; j < n; j++) {
        sum = 0;
        r.forEach((x) => { sum += x; });
        global_res = sum;
    }
    return n * len;
}

/* Array.prototype.reduce over a hundred numbers, with an initial value */
function array_reduce(n)
{
    var r, i, j, len = 100;
    r = [];
    for (i = 0; i < len; i++)
        r[i] = i;
    for (j = 0; j < n; j++)
        global_res = r.reduce((acc, x) => acc + x, 0);
    return n * len;
}

/* Array.prototype.map over a hundred numbers, into a new array of them */
function array_map(n)
{
    var r, i, j, len = 100;
    r = [];
    for (i = 0; i < len; i++)
        r[i] = i;
    for (j = 0; j < n; j++)
        global_res = r.map((x) => x * 2);
    return n * len;
}

/* Array.prototype.filter over a hundred numbers, keeping half */
function array_filter(n)
{
    var r, i, j, len = 100;
    r = [];
    for (i = 0; i < len; i++)
        r[i] = i;
    for (j = 0; j < n; j++)
        global_res = r.filter((x) => (x & 1) === 0);
    return n * len;
}

/* String.prototype.indexOf of each character of a text in an alphabet,
   as a base64 decoder looks its characters up */
function string_indexOf(n)
{
    var alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    var text = "SGVsbG8sIHdvcmxkISBUaGlzIGlzIGEgdGVzdCBvZiBpbmRleE9mLg";
    var i, j, s, len = text.length;
    for (j = 0; j < n; j++) {
        s = 0;
        for (i = 0; i < len; i++)
            s += alphabet.indexOf(text.charAt(i));
        global_res = s;
    }
    return n * len;
}

/* Array.prototype.indexOf of a number in an array of a hundred, found
   half way along */
function array_indexOf(n)
{
    var r, i, j, len = 100;
    r = [];
    for (i = 0; i < len; i++)
        r[i] = i;
    for (j = 0; j < n; j++)
        global_res = r.indexOf(j & 1 ? 50 : 49);
    return n;
}

/* String.prototype's methods that run no script on a string, each called
   with primitives in a loop: at, codePointAt, toUpperCase, trim,
   startsWith, padStart, repeat, slice and concat of several characters,
   String.fromCodePoint */
var extra_str = "Hello, World of strings";

function string_at(n)
{
    var j, s = 0;
    for (j = 0; j < n; j++)
        s += extra_str.at(j & 15).length;
    global_res = s;
    return n;
}

function string_code_point_at(n)
{
    var j, s = 0;
    for (j = 0; j < n; j++)
        s += extra_str.codePointAt(j & 15);
    global_res = s;
    return n;
}

function string_upper(n)
{
    var j, s = 0;
    for (j = 0; j < n; j++)
        s += extra_str.toUpperCase().length;
    global_res = s;
    return n;
}

function string_trim(n)
{
    var j, s = 0, t = "  padded  ";
    for (j = 0; j < n; j++)
        s += t.trim().length;
    global_res = s;
    return n;
}

function string_starts_with(n)
{
    var j, s = 0;
    for (j = 0; j < n; j++) {
        if (extra_str.startsWith("Hello", j & 1))
            s++;
    }
    global_res = s;
    return n;
}

function string_pad_start(n)
{
    var j, s = 0;
    for (j = 0; j < n; j++)
        s += String(j & 7).padStart(4, "0").length;
    global_res = s;
    return n;
}

function string_repeat(n)
{
    var j, s = 0;
    for (j = 0; j < n; j++)
        s += "ab".repeat((j & 3) + 1).length;
    global_res = s;
    return n;
}

function string_slice(n)
{
    var j, s = 0;
    for (j = 0; j < n; j++)
        s += extra_str.slice(2, 9 + (j & 3)).length;
    global_res = s;
    return n;
}

function string_concat(n)
{
    var j, s = 0;
    for (j = 0; j < n; j++)
        s += extra_str.concat("!", j & 7).length;
    global_res = s;
    return n;
}

function string_from_code_point(n)
{
    var j, s = 0;
    for (j = 0; j < n; j++)
        s += String.fromCodePoint(65 + (j & 7), 0x1F600).length;
    global_res = s;
    return n;
}

/* HTML escaping: check for a match, then scan, slice and join replacements. */
var extra_html_escape_re = /["'&<>]/;

function extra_html_escape(n)
{
    if (typeof n == "boolean" || typeof n == "number" || typeof n == "bigint")
        return "" + n;
    n = "" + n;
    var e = extra_html_escape_re.exec(n);
    if (e) {
        var t = "", l, r = 0;
        for (l = e.index; l < n.length; l++) {
            switch (n.charCodeAt(l)) {
            case 34: e = "&quot;"; break;
            case 38: e = "&amp;"; break;
            case 39: e = "&#x27;"; break;
            case 60: e = "&lt;"; break;
            case 62: e = "&gt;"; break;
            default: continue;
            }
            r !== l && (t += n.slice(r, l)), r = l + 1, t += e;
        }
        n = r !== l ? t + n.slice(r, l) : t;
    }
    return n;
}

var extra_html_plain = ["article", "product-card", "linen", "A plain text value"];
var extra_html_entities = [
    "Tom & Jerry", '<a title="x">\'sale\'</a>', '"\'&<>',
    "A long plain prefix before < and a long plain suffix after &"
];
var extra_html_mixed = [
    "article", "product-card", "linen", "Tom & Jerry",
    '<a title="x">\'sale\'</a>', "", 42, true
];

function html_escape_plain(n)
{
    var j, s = 0;
    for (j = 0; j < n; j++)
        s += extra_html_escape(extra_html_plain[j & 3]).length;
    global_res = s;
    return n;
}

function html_escape_entities(n)
{
    var j, s = 0;
    for (j = 0; j < n; j++)
        s += extra_html_escape(extra_html_entities[j & 3]).length;
    global_res = s;
    return n;
}

function html_escape_mixed(n)
{
    var j, s = 0;
    for (j = 0; j < n; j++)
        s += extra_html_escape(extra_html_mixed[j & 7]).length;
    global_res = s;
    return n;
}

/* %TypedArray%.prototype.forEach over an Int32Array of a hundred, its
   callback an arrow that adds to a binding it captured */
function typed_forEach(n)
{
    var r, i, j, sum, len = 100;
    r = new Int32Array(len);
    for (i = 0; i < len; i++)
        r[i] = i;
    for (j = 0; j < n; j++) {
        sum = 0;
        r.forEach((x) => { sum += x; });
        global_res = sum;
    }
    return n * len;
}

/* an Int32Array of a hundred, 0 to 99, as typed_forEach's */
function extra_int32s(len)
{
    var r = new Int32Array(len);
    for (var i = 0; i < len; i++)
        r[i] = i;
    return r;
}

/* %TypedArray%.prototype.map into a new Int32Array of a hundred */
function typed_map(n)
{
    var r, j, len = 100;
    r = extra_int32s(len);
    for (j = 0; j < n; j++)
        global_res = r.map((x) => x * 2);
    return n * len;
}

/* %TypedArray%.prototype.filter of a hundred, keeping half */
function typed_filter(n)
{
    var r, j, len = 100;
    r = extra_int32s(len);
    for (j = 0; j < n; j++)
        global_res = r.filter((x) => (x & 1) === 0);
    return n * len;
}

/* %TypedArray%.prototype.reduce of a hundred, with an initial value */
function typed_reduce(n)
{
    var r, j, len = 100;
    r = extra_int32s(len);
    for (j = 0; j < n; j++)
        global_res = r.reduce((acc, x) => acc + x, 0);
    return n * len;
}

/* %TypedArray%.prototype.some of a hundred, the one it looks for the last */
function typed_some(n)
{
    var r, j, len = 100;
    r = extra_int32s(len);
    for (j = 0; j < n; j++)
        global_res = r.some((x) => x === len - 1);
    return n * len;
}

/* %TypedArray%.prototype.every of a hundred, each true */
function typed_every(n)
{
    var r, j, len = 100;
    r = extra_int32s(len);
    for (j = 0; j < n; j++)
        global_res = r.every((x) => x < len);
    return n * len;
}

/* %TypedArray%.prototype.find of the last of a hundred */
function typed_find(n)
{
    var r, j, len = 100;
    r = extra_int32s(len);
    for (j = 0; j < n; j++)
        global_res = r.find((x) => x === len - 1);
    return n * len;
}

/* %TypedArray%.prototype.findIndex of the last of a hundred */
function typed_findIndex(n)
{
    var r, j, len = 100;
    r = extra_int32s(len);
    for (j = 0; j < n; j++)
        global_res = r.findIndex((x) => x === len - 1);
    return n * len;
}

/* %TypedArray%.prototype.sort of a hundred, a permutation of 0 to 99
   copied in each turn, by a comparator; an operation is an element sorted,
   whatever comparisons the engine's sort makes */
function typed_sort(n)
{
    var r, s, i, j, len = 100;
    s = new Int32Array(len);
    for (i = 0; i < len; i++)
        s[i] = (i * 37) % len;
    r = new Int32Array(len);
    for (j = 0; j < n; j++) {
        r.set(s);
        r.sort((a, b) => a - b);
    }
    global_res = r[len - 1];
    return n * len;
}

/* The typed array tests above over each element type: forEach and reduce
   over all of them, the other callback methods over a Uint8Array, a
   Float64Array and a BigInt64Array, and typed_array_read's and
   typed_array_write's loops, which are an Int32Array's. */

/* a Int8Array of -50 to 49 */
function extra_i8s(len)
{
    var r = new Int8Array(len);
    for (var i = 0; i < len; i++)
        r[i] = i - 50;
    return r;
}

/* a Uint8Array of 50 to 248, as a byte buffer's */
function extra_u8s(len)
{
    var r = new Uint8Array(len);
    for (var i = 0; i < len; i++)
        r[i] = i * 2 + 50;
    return r;
}

/* a Uint8ClampedArray of 50 to 248, as an image's pixels */
function extra_u8cs(len)
{
    var r = new Uint8ClampedArray(len);
    for (var i = 0; i < len; i++)
        r[i] = i * 2 + 50;
    return r;
}

/* a Int16Array of -30000 to 29400 */
function extra_i16s(len)
{
    var r = new Int16Array(len);
    for (var i = 0; i < len; i++)
        r[i] = (i - 50) * 600;
    return r;
}

/* a Uint16Array of 0 to 64350 */
function extra_u16s(len)
{
    var r = new Uint16Array(len);
    for (var i = 0; i < len; i++)
        r[i] = i * 650;
    return r;
}

/* a Uint32Array whose odd elements are above 2**31, no int32s */
function extra_u32s(len)
{
    var r = new Uint32Array(len);
    for (var i = 0; i < len; i++)
        r[i] = i & 1 ? 0x80000000 + i : i;
    return r;
}

/* a Float32Array of i / 4 + 0.1, each rounded to float32 */
function extra_f32s(len)
{
    var r = new Float32Array(len);
    for (var i = 0; i < len; i++)
        r[i] = i * 0.25 + 0.1;
    return r;
}

/* a Float64Array of i / 4 + 0.1 */
function extra_f64s(len)
{
    var r = new Float64Array(len);
    for (var i = 0; i < len; i++)
        r[i] = i * 0.25 + 0.1;
    return r;
}

/* a BigInt64Array of (i - 50) * 10**12 */
function extra_i64s(len)
{
    var r = new BigInt64Array(len);
    for (var i = 0; i < len; i++)
        r[i] = BigInt(i - 50) * 1000000000000n;
    return r;
}

/* a BigUint64Array whose odd elements are above 2**63 */
function extra_u64s(len)
{
    var r = new BigUint64Array(len);
    for (var i = 0; i < len; i++)
        r[i] = i & 1 ? 2n ** 63n + BigInt(i) : BigInt(i);
    return r;
}

/* typed_forEach over extra_i8s */
function typed_i8_forEach(n)
{
    var r, j, sum, len = 100;
    r = extra_i8s(len);
    for (j = 0; j < n; j++) {
        sum = 0;
        r.forEach((x) => { sum += x; });
        global_res = sum;
    }
    return n * len;
}

/* typed_reduce over extra_i8s */
function typed_i8_reduce(n)
{
    var r, j, len = 100;
    r = extra_i8s(len);
    for (j = 0; j < n; j++)
        global_res = r.reduce((acc, x) => acc + x, 0);
    return n * len;
}

/* typed_array_read over a Int8Array of ten, as extra_i8s fills it */
function typed_i8_read(n)
{
    var tab, sum, j, len = 10;
    tab = extra_i8s(len);
    sum = 0;
    for (j = 0; j < n; j++) {
        sum += tab[0];
        sum += tab[1];
        sum += tab[2];
        sum += tab[3];
        sum += tab[4];
        sum += tab[5];
        sum += tab[6];
        sum += tab[7];
        sum += tab[8];
        sum += tab[9];
    }
    global_res = sum;
    return len * n;
}

/* typed_array_write over a Int8Array of ten, of j wrapped to a byte */
function typed_i8_write(n)
{
    var tab, j, v = 0, len = 10;
    tab = extra_i8s(len);
    for (j = 0; j < n; j++) {
        v = j;
        tab[0] = v;
        tab[1] = v;
        tab[2] = v;
        tab[3] = v;
        tab[4] = v;
        tab[5] = v;
        tab[6] = v;
        tab[7] = v;
        tab[8] = v;
        tab[9] = v;
    }
    global_res = tab[9];
    return len * n;
}

/* typed_forEach over extra_u8s */
function typed_u8_forEach(n)
{
    var r, j, sum, len = 100;
    r = extra_u8s(len);
    for (j = 0; j < n; j++) {
        sum = 0;
        r.forEach((x) => { sum += x; });
        global_res = sum;
    }
    return n * len;
}

/* typed_reduce over extra_u8s */
function typed_u8_reduce(n)
{
    var r, j, len = 100;
    r = extra_u8s(len);
    for (j = 0; j < n; j++)
        global_res = r.reduce((acc, x) => acc + x, 0);
    return n * len;
}

/* typed_map over extra_u8s */
function typed_u8_map(n)
{
    var r, j, len = 100;
    r = extra_u8s(len);
    for (j = 0; j < n; j++)
        global_res = r.map((x) => x * 2);
    return n * len;
}

/* typed_filter over extra_u8s, keeping the even indices */
function typed_u8_filter(n)
{
    var r, j, len = 100;
    r = extra_u8s(len);
    for (j = 0; j < n; j++)
        global_res = r.filter((x, i) => (i & 1) === 0);
    return n * len;
}

/* typed_every over extra_u8s, each no more than the last */
function typed_u8_every(n)
{
    var r, j, last, len = 100;
    r = extra_u8s(len);
    last = r[len - 1];
    for (j = 0; j < n; j++)
        global_res = r.every((x) => x <= last);
    return n * len;
}

/* typed_find over extra_u8s, of the last */
function typed_u8_find(n)
{
    var r, j, last, len = 100;
    r = extra_u8s(len);
    last = r[len - 1];
    for (j = 0; j < n; j++)
        global_res = r.find((x) => x === last);
    return n * len;
}

/* typed_sort over extra_u8s, permuted */
function typed_u8_sort(n)
{
    var r, s, i, j, len = 100;
    r = extra_u8s(len);
    s = new Uint8Array(len);
    for (i = 0; i < len; i++)
        s[i] = r[(i * 37) % len];
    for (j = 0; j < n; j++) {
        r.set(s);
        r.sort((a, b) => a - b);
    }
    global_res = r[len - 1];
    return n * len;
}

/* typed_array_read over a Uint8Array of ten, as extra_u8s fills it */
function typed_u8_read(n)
{
    var tab, sum, j, len = 10;
    tab = extra_u8s(len);
    sum = 0;
    for (j = 0; j < n; j++) {
        sum += tab[0];
        sum += tab[1];
        sum += tab[2];
        sum += tab[3];
        sum += tab[4];
        sum += tab[5];
        sum += tab[6];
        sum += tab[7];
        sum += tab[8];
        sum += tab[9];
    }
    global_res = sum;
    return len * n;
}

/* typed_array_write over a Uint8Array of ten, of j wrapped to a byte */
function typed_u8_write(n)
{
    var tab, j, v = 0, len = 10;
    tab = extra_u8s(len);
    for (j = 0; j < n; j++) {
        v = j;
        tab[0] = v;
        tab[1] = v;
        tab[2] = v;
        tab[3] = v;
        tab[4] = v;
        tab[5] = v;
        tab[6] = v;
        tab[7] = v;
        tab[8] = v;
        tab[9] = v;
    }
    global_res = tab[9];
    return len * n;
}

/* typed_forEach over extra_u8cs */
function typed_u8c_forEach(n)
{
    var r, j, sum, len = 100;
    r = extra_u8cs(len);
    for (j = 0; j < n; j++) {
        sum = 0;
        r.forEach((x) => { sum += x; });
        global_res = sum;
    }
    return n * len;
}

/* typed_reduce over extra_u8cs */
function typed_u8c_reduce(n)
{
    var r, j, len = 100;
    r = extra_u8cs(len);
    for (j = 0; j < n; j++)
        global_res = r.reduce((acc, x) => acc + x, 0);
    return n * len;
}

/* typed_array_read over a Uint8ClampedArray of ten, as extra_u8cs fills it */
function typed_u8c_read(n)
{
    var tab, sum, j, len = 10;
    tab = extra_u8cs(len);
    sum = 0;
    for (j = 0; j < n; j++) {
        sum += tab[0];
        sum += tab[1];
        sum += tab[2];
        sum += tab[3];
        sum += tab[4];
        sum += tab[5];
        sum += tab[6];
        sum += tab[7];
        sum += tab[8];
        sum += tab[9];
    }
    global_res = sum;
    return len * n;
}

/* typed_array_write over a Uint8ClampedArray of ten, of -128 to 383, clamped to 0 to 255 */
function typed_u8c_write(n)
{
    var tab, j, v = 0, len = 10;
    tab = extra_u8cs(len);
    for (j = 0; j < n; j++) {
        v = (j & 511) - 128;
        tab[0] = v;
        tab[1] = v;
        tab[2] = v;
        tab[3] = v;
        tab[4] = v;
        tab[5] = v;
        tab[6] = v;
        tab[7] = v;
        tab[8] = v;
        tab[9] = v;
    }
    global_res = tab[9];
    return len * n;
}

/* typed_forEach over extra_i16s */
function typed_i16_forEach(n)
{
    var r, j, sum, len = 100;
    r = extra_i16s(len);
    for (j = 0; j < n; j++) {
        sum = 0;
        r.forEach((x) => { sum += x; });
        global_res = sum;
    }
    return n * len;
}

/* typed_reduce over extra_i16s */
function typed_i16_reduce(n)
{
    var r, j, len = 100;
    r = extra_i16s(len);
    for (j = 0; j < n; j++)
        global_res = r.reduce((acc, x) => acc + x, 0);
    return n * len;
}

/* typed_array_read over a Int16Array of ten, as extra_i16s fills it */
function typed_i16_read(n)
{
    var tab, sum, j, len = 10;
    tab = extra_i16s(len);
    sum = 0;
    for (j = 0; j < n; j++) {
        sum += tab[0];
        sum += tab[1];
        sum += tab[2];
        sum += tab[3];
        sum += tab[4];
        sum += tab[5];
        sum += tab[6];
        sum += tab[7];
        sum += tab[8];
        sum += tab[9];
    }
    global_res = sum;
    return len * n;
}

/* typed_array_write over a Int16Array of ten, of j wrapped */
function typed_i16_write(n)
{
    var tab, j, v = 0, len = 10;
    tab = extra_i16s(len);
    for (j = 0; j < n; j++) {
        v = j;
        tab[0] = v;
        tab[1] = v;
        tab[2] = v;
        tab[3] = v;
        tab[4] = v;
        tab[5] = v;
        tab[6] = v;
        tab[7] = v;
        tab[8] = v;
        tab[9] = v;
    }
    global_res = tab[9];
    return len * n;
}

/* typed_forEach over extra_u16s */
function typed_u16_forEach(n)
{
    var r, j, sum, len = 100;
    r = extra_u16s(len);
    for (j = 0; j < n; j++) {
        sum = 0;
        r.forEach((x) => { sum += x; });
        global_res = sum;
    }
    return n * len;
}

/* typed_reduce over extra_u16s */
function typed_u16_reduce(n)
{
    var r, j, len = 100;
    r = extra_u16s(len);
    for (j = 0; j < n; j++)
        global_res = r.reduce((acc, x) => acc + x, 0);
    return n * len;
}

/* typed_array_read over a Uint16Array of ten, as extra_u16s fills it */
function typed_u16_read(n)
{
    var tab, sum, j, len = 10;
    tab = extra_u16s(len);
    sum = 0;
    for (j = 0; j < n; j++) {
        sum += tab[0];
        sum += tab[1];
        sum += tab[2];
        sum += tab[3];
        sum += tab[4];
        sum += tab[5];
        sum += tab[6];
        sum += tab[7];
        sum += tab[8];
        sum += tab[9];
    }
    global_res = sum;
    return len * n;
}

/* typed_array_write over a Uint16Array of ten, of j wrapped */
function typed_u16_write(n)
{
    var tab, j, v = 0, len = 10;
    tab = extra_u16s(len);
    for (j = 0; j < n; j++) {
        v = j;
        tab[0] = v;
        tab[1] = v;
        tab[2] = v;
        tab[3] = v;
        tab[4] = v;
        tab[5] = v;
        tab[6] = v;
        tab[7] = v;
        tab[8] = v;
        tab[9] = v;
    }
    global_res = tab[9];
    return len * n;
}

/* typed_forEach over extra_u32s */
function typed_u32_forEach(n)
{
    var r, j, sum, len = 100;
    r = extra_u32s(len);
    for (j = 0; j < n; j++) {
        sum = 0;
        r.forEach((x) => { sum += x; });
        global_res = sum;
    }
    return n * len;
}

/* typed_reduce over extra_u32s */
function typed_u32_reduce(n)
{
    var r, j, len = 100;
    r = extra_u32s(len);
    for (j = 0; j < n; j++)
        global_res = r.reduce((acc, x) => acc + x, 0);
    return n * len;
}

/* typed_array_read over a Uint32Array of ten, as extra_u32s fills it */
function typed_u32_read(n)
{
    var tab, sum, j, len = 10;
    tab = extra_u32s(len);
    sum = 0;
    for (j = 0; j < n; j++) {
        sum += tab[0];
        sum += tab[1];
        sum += tab[2];
        sum += tab[3];
        sum += tab[4];
        sum += tab[5];
        sum += tab[6];
        sum += tab[7];
        sum += tab[8];
        sum += tab[9];
    }
    global_res = sum;
    return len * n;
}

/* typed_array_write over a Uint32Array of ten, of above 2**31 */
function typed_u32_write(n)
{
    var tab, j, v = 0, len = 10;
    tab = extra_u32s(len);
    for (j = 0; j < n; j++) {
        v = j + 0x80000000;
        tab[0] = v;
        tab[1] = v;
        tab[2] = v;
        tab[3] = v;
        tab[4] = v;
        tab[5] = v;
        tab[6] = v;
        tab[7] = v;
        tab[8] = v;
        tab[9] = v;
    }
    global_res = tab[9];
    return len * n;
}

/* typed_forEach over extra_f32s */
function typed_f32_forEach(n)
{
    var r, j, sum, len = 100;
    r = extra_f32s(len);
    for (j = 0; j < n; j++) {
        sum = 0;
        r.forEach((x) => { sum += x; });
        global_res = sum;
    }
    return n * len;
}

/* typed_reduce over extra_f32s */
function typed_f32_reduce(n)
{
    var r, j, len = 100;
    r = extra_f32s(len);
    for (j = 0; j < n; j++)
        global_res = r.reduce((acc, x) => acc + x, 0);
    return n * len;
}

/* typed_array_read over a Float32Array of ten, as extra_f32s fills it */
function typed_f32_read(n)
{
    var tab, sum, j, len = 10;
    tab = extra_f32s(len);
    sum = 0;
    for (j = 0; j < n; j++) {
        sum += tab[0];
        sum += tab[1];
        sum += tab[2];
        sum += tab[3];
        sum += tab[4];
        sum += tab[5];
        sum += tab[6];
        sum += tab[7];
        sum += tab[8];
        sum += tab[9];
    }
    global_res = sum;
    return len * n;
}

/* typed_array_write over a Float32Array of ten, of j / 10, rounded to float32 */
function typed_f32_write(n)
{
    var tab, j, v = 0, len = 10;
    tab = extra_f32s(len);
    for (j = 0; j < n; j++) {
        v = j * 0.1;
        tab[0] = v;
        tab[1] = v;
        tab[2] = v;
        tab[3] = v;
        tab[4] = v;
        tab[5] = v;
        tab[6] = v;
        tab[7] = v;
        tab[8] = v;
        tab[9] = v;
    }
    global_res = tab[9];
    return len * n;
}

/* typed_forEach over extra_f64s */
function typed_f64_forEach(n)
{
    var r, j, sum, len = 100;
    r = extra_f64s(len);
    for (j = 0; j < n; j++) {
        sum = 0;
        r.forEach((x) => { sum += x; });
        global_res = sum;
    }
    return n * len;
}

/* typed_reduce over extra_f64s */
function typed_f64_reduce(n)
{
    var r, j, len = 100;
    r = extra_f64s(len);
    for (j = 0; j < n; j++)
        global_res = r.reduce((acc, x) => acc + x, 0);
    return n * len;
}

/* typed_map over extra_f64s */
function typed_f64_map(n)
{
    var r, j, len = 100;
    r = extra_f64s(len);
    for (j = 0; j < n; j++)
        global_res = r.map((x) => x * 2);
    return n * len;
}

/* typed_filter over extra_f64s, keeping the even indices */
function typed_f64_filter(n)
{
    var r, j, len = 100;
    r = extra_f64s(len);
    for (j = 0; j < n; j++)
        global_res = r.filter((x, i) => (i & 1) === 0);
    return n * len;
}

/* typed_every over extra_f64s, each no more than the last */
function typed_f64_every(n)
{
    var r, j, last, len = 100;
    r = extra_f64s(len);
    last = r[len - 1];
    for (j = 0; j < n; j++)
        global_res = r.every((x) => x <= last);
    return n * len;
}

/* typed_find over extra_f64s, of the last */
function typed_f64_find(n)
{
    var r, j, last, len = 100;
    r = extra_f64s(len);
    last = r[len - 1];
    for (j = 0; j < n; j++)
        global_res = r.find((x) => x === last);
    return n * len;
}

/* typed_sort over extra_f64s, permuted */
function typed_f64_sort(n)
{
    var r, s, i, j, len = 100;
    r = extra_f64s(len);
    s = new Float64Array(len);
    for (i = 0; i < len; i++)
        s[i] = r[(i * 37) % len];
    for (j = 0; j < n; j++) {
        r.set(s);
        r.sort((a, b) => a - b);
    }
    global_res = r[len - 1];
    return n * len;
}

/* typed_array_read over a Float64Array of ten, as extra_f64s fills it */
function typed_f64_read(n)
{
    var tab, sum, j, len = 10;
    tab = extra_f64s(len);
    sum = 0;
    for (j = 0; j < n; j++) {
        sum += tab[0];
        sum += tab[1];
        sum += tab[2];
        sum += tab[3];
        sum += tab[4];
        sum += tab[5];
        sum += tab[6];
        sum += tab[7];
        sum += tab[8];
        sum += tab[9];
    }
    global_res = sum;
    return len * n;
}

/* typed_array_write over a Float64Array of ten, of j / 10 */
function typed_f64_write(n)
{
    var tab, j, v = 0, len = 10;
    tab = extra_f64s(len);
    for (j = 0; j < n; j++) {
        v = j * 0.1;
        tab[0] = v;
        tab[1] = v;
        tab[2] = v;
        tab[3] = v;
        tab[4] = v;
        tab[5] = v;
        tab[6] = v;
        tab[7] = v;
        tab[8] = v;
        tab[9] = v;
    }
    global_res = tab[9];
    return len * n;
}

/* typed_forEach over extra_i64s */
function typed_i64_forEach(n)
{
    var r, j, sum, len = 100;
    r = extra_i64s(len);
    for (j = 0; j < n; j++) {
        sum = 0n;
        r.forEach((x) => { sum += x; });
        global_res = sum;
    }
    return n * len;
}

/* typed_reduce over extra_i64s */
function typed_i64_reduce(n)
{
    var r, j, len = 100;
    r = extra_i64s(len);
    for (j = 0; j < n; j++)
        global_res = r.reduce((acc, x) => acc + x, 0n);
    return n * len;
}

/* typed_map over extra_i64s */
function typed_i64_map(n)
{
    var r, j, len = 100;
    r = extra_i64s(len);
    for (j = 0; j < n; j++)
        global_res = r.map((x) => x * 2n);
    return n * len;
}

/* typed_filter over extra_i64s, keeping the even indices */
function typed_i64_filter(n)
{
    var r, j, len = 100;
    r = extra_i64s(len);
    for (j = 0; j < n; j++)
        global_res = r.filter((x, i) => (i & 1) === 0);
    return n * len;
}

/* typed_every over extra_i64s, each no more than the last */
function typed_i64_every(n)
{
    var r, j, last, len = 100;
    r = extra_i64s(len);
    last = r[len - 1];
    for (j = 0; j < n; j++)
        global_res = r.every((x) => x <= last);
    return n * len;
}

/* typed_find over extra_i64s, of the last */
function typed_i64_find(n)
{
    var r, j, last, len = 100;
    r = extra_i64s(len);
    last = r[len - 1];
    for (j = 0; j < n; j++)
        global_res = r.find((x) => x === last);
    return n * len;
}

/* typed_sort over extra_i64s, permuted */
function typed_i64_sort(n)
{
    var r, s, i, j, len = 100;
    r = extra_i64s(len);
    s = new BigInt64Array(len);
    for (i = 0; i < len; i++)
        s[i] = r[(i * 37) % len];
    for (j = 0; j < n; j++) {
        r.set(s);
        r.sort((a, b) => (a < b ? -1 : a > b ? 1 : 0));
    }
    global_res = r[len - 1];
    return n * len;
}

/* typed_array_read over a BigInt64Array of ten, as extra_i64s fills it */
function typed_i64_read(n)
{
    var tab, sum, j, len = 10;
    tab = extra_i64s(len);
    sum = 0n;
    for (j = 0; j < n; j++) {
        sum += tab[0];
        sum += tab[1];
        sum += tab[2];
        sum += tab[3];
        sum += tab[4];
        sum += tab[5];
        sum += tab[6];
        sum += tab[7];
        sum += tab[8];
        sum += tab[9];
    }
    global_res = sum;
    return len * n;
}

/* typed_array_write over a BigInt64Array of ten, of a BigInt stepped by 3 */
function typed_i64_write(n)
{
    var tab, j, v = 0n, len = 10;
    tab = extra_i64s(len);
    for (j = 0; j < n; j++) {
        v += 3n;
        tab[0] = v;
        tab[1] = v;
        tab[2] = v;
        tab[3] = v;
        tab[4] = v;
        tab[5] = v;
        tab[6] = v;
        tab[7] = v;
        tab[8] = v;
        tab[9] = v;
    }
    global_res = tab[9];
    return len * n;
}

/* typed_forEach over extra_u64s */
function typed_u64_forEach(n)
{
    var r, j, sum, len = 100;
    r = extra_u64s(len);
    for (j = 0; j < n; j++) {
        sum = 0n;
        r.forEach((x) => { sum += x; });
        global_res = sum;
    }
    return n * len;
}

/* typed_reduce over extra_u64s */
function typed_u64_reduce(n)
{
    var r, j, len = 100;
    r = extra_u64s(len);
    for (j = 0; j < n; j++)
        global_res = r.reduce((acc, x) => acc + x, 0n);
    return n * len;
}

/* typed_array_read over a BigUint64Array of ten, as extra_u64s fills it */
function typed_u64_read(n)
{
    var tab, sum, j, len = 10;
    tab = extra_u64s(len);
    sum = 0n;
    for (j = 0; j < n; j++) {
        sum += tab[0];
        sum += tab[1];
        sum += tab[2];
        sum += tab[3];
        sum += tab[4];
        sum += tab[5];
        sum += tab[6];
        sum += tab[7];
        sum += tab[8];
        sum += tab[9];
    }
    global_res = sum;
    return len * n;
}

/* typed_array_write over a BigUint64Array of ten, of a BigInt above 2**63 stepped by 3 */
function typed_u64_write(n)
{
    var tab, j, v = 2n ** 63n, len = 10;
    tab = extra_u64s(len);
    for (j = 0; j < n; j++) {
        v += 3n;
        tab[0] = v;
        tab[1] = v;
        tab[2] = v;
        tab[3] = v;
        tab[4] = v;
        tab[5] = v;
        tab[6] = v;
        tab[7] = v;
        tab[8] = v;
        tab[9] = v;
    }
    global_res = tab[9];
    return len * n;
}

/* a DataView's getters over a record of 64 bytes, as a binary format's
   reader takes its fields: bytes, little-endian int32s and float64s */
function extra_record()
{
    var v = new DataView(new ArrayBuffer(64));
    for (var i = 0; i < 64; i++)
        v.setUint8(i, i * 7);
    v.setFloat64(16, 1.5, true);
    v.setFloat64(24, -2.25, true);
    v.setFloat64(32, 1e10, true);
    return v;
}

function dataview_read(n)
{
    var v, j, sum = 0;
    v = extra_record();
    for (j = 0; j < n; j++) {
        sum += v.getUint8(0);
        sum += v.getUint8(1);
        sum += v.getUint8(2);
        sum += v.getUint8(3);
        sum += v.getInt32(4, true);
        sum += v.getInt32(8, true);
        sum += v.getInt32(12, true);
        sum += v.getFloat64(16, true);
        sum += v.getFloat64(24, true);
        sum += v.getFloat64(32, true);
    }
    global_res = sum;
    return 10 * n;
}

/* a DataView's setters, as dataview_read's getters */
function dataview_write(n)
{
    var v, j, x;
    v = extra_record();
    for (j = 0; j < n; j++) {
        x = j * 0.5;
        v.setUint8(0, j);
        v.setUint8(1, j);
        v.setUint8(2, j);
        v.setUint8(3, j);
        v.setInt32(4, j, true);
        v.setInt32(8, j, true);
        v.setInt32(12, j, true);
        v.setFloat64(16, x, true);
        v.setFloat64(24, x, true);
        v.setFloat64(32, x, true);
    }
    global_res = v.getFloat64(16, true);
    return 10 * n;
}

/* Map.prototype.forEach over a hundred entries, its callback an arrow made
   anew at each call that adds to a binding it captured */
function map_forEach(n)
{
    var m, i, j, sum, len = 100;
    m = new Map();
    for (i = 0; i < len; i++)
        m.set(i, i);
    for (j = 0; j < n; j++) {
        sum = 0;
        m.forEach((v, k) => { sum += v; });
        global_res = sum;
    }
    return n * len;
}

/* Set.prototype.forEach over a hundred numbers, as map_forEach */
function set_forEach(n)
{
    var s, i, j, sum, len = 100;
    s = new Set();
    for (i = 0; i < len; i++)
        s.add(i);
    for (j = 0; j < n; j++) {
        sum = 0;
        s.forEach((x) => { sum += x; });
        global_res = sum;
    }
    return n * len;
}

/* Array.prototype.some over a hundred numbers, the one it looks for the
   last, as every, find and findIndex walk an array */
function array_some(n)
{
    var r, i, j, len = 100;
    r = [];
    for (i = 0; i < len; i++)
        r[i] = i;
    for (j = 0; j < n; j++)
        global_res = r.some((x) => x === len - 1);
    return n * len;
}

/* The callbacks a built-in calls for each element, as array_forEach and
   typed_forEach have them -- an arrow made anew at each call that writes
   a binding it captured -- each varied in one thing: made once, a function
   expression, an arrow reading this in a method, a loop of the script's own
   calling it, other methods calling it, a typed array's, a Map's or a Set's
   method. */

/* array_forEach's arrow made once, before the loop */
function array_forEach_once(n)
{
    var r, i, j, sum, add, len = 100;
    r = [];
    for (i = 0; i < len; i++)
        r[i] = i;
    add = (x) => { sum += x; };
    for (j = 0; j < n; j++) {
        sum = 0;
        r.forEach(add);
        global_res = sum;
    }
    return n * len;
}

/* array_forEach's callback a function expression rather than an arrow */
function array_forEach_function(n)
{
    var r, i, j, sum, len = 100;
    r = [];
    for (i = 0; i < len; i++)
        r[i] = i;
    for (j = 0; j < n; j++) {
        sum = 0;
        r.forEach(function (x) { sum += x; });
        global_res = sum;
    }
    return n * len;
}

/* a method whose forEach callback, an arrow made at each call, adds to a
   property of the method's this */
class ExtraTally
{
    constructor(len)
    {
        this.sum = 0;
        this.items = [];
        for (var i = 0; i < len; i++)
            this.items[i] = i;
    }
    total()
    {
        this.sum = 0;
        this.items.forEach((x) => { this.sum += x; });
        return this.sum;
    }
}

function array_forEach_this(n)
{
    var j, len = 100, t = new ExtraTally(len);
    for (j = 0; j < n; j++)
        global_res = t.total();
    return n * len;
}

/* array_forEach's arrow called by a loop of the script's own rather than
   by the built-in */
function extra_forEach(a, f)
{
    for (var i = 0; i < a.length; i++)
        f(a[i], i, a);
}

function array_forEach_user(n)
{
    var r, i, j, sum, len = 100;
    r = [];
    for (i = 0; i < len; i++)
        r[i] = i;
    for (j = 0; j < n; j++) {
        sum = 0;
        extra_forEach(r, (x) => { sum += x; });
        global_res = sum;
    }
    return n * len;
}

/* array_reduce's callback also counting its calls in a captured binding */
function array_reduce_upvalue(n)
{
    var r, i, j, calls = 0, len = 100;
    r = [];
    for (i = 0; i < len; i++)
        r[i] = i;
    for (j = 0; j < n; j++)
        global_res = r.reduce((acc, x) => { calls++; return acc + x; }, 0);
    global_res += calls;
    return n * len;
}

/* Array.prototype.every over a hundred numbers, each true, its callback
   keeping the last it was given in a captured binding */
function array_every_upvalue(n)
{
    var r, i, j, last = 0, len = 100;
    r = [];
    for (i = 0; i < len; i++)
        r[i] = i;
    for (j = 0; j < n; j++)
        global_res = r.every((x) => { last = x; return x < len; });
    global_res += last;
    return n * len;
}

/* Array.prototype.find of the last of a hundred numbers, its callback
   adding each it is given to a captured binding */
function array_find_upvalue(n)
{
    var r, i, j, seen, len = 100;
    r = [];
    for (i = 0; i < len; i++)
        r[i] = i;
    for (j = 0; j < n; j++) {
        seen = 0;
        global_res = r.find((x) => { seen += x; return x === len - 1; }) + seen;
    }
    return n * len;
}

/* Array.prototype.sort of a hundred numbers, a permutation copied in each
   turn, by a comparator, as typed_sort sorts an Int32Array's; an operation
   is an element sorted, whatever comparisons the engine's sort makes */
function array_sort(n)
{
    var r, s, i, j, len = 100;
    s = [];
    for (i = 0; i < len; i++)
        s[i] = (i * 37) % len;
    for (j = 0; j < n; j++) {
        r = s.slice();
        r.sort((a, b) => a - b);
    }
    global_res = r[len - 1];
    return n * len;
}

/* digit sums of numbers of nineteen digits: a remainder and a division in
   the int64 range a digit */
function bigint64_divmod(n)
{
    var j, x, s;
    for (j = 0; j < n; j++) {
        x = 9007199254740993000n + BigInt(j);
        s = 0n;
        while (x > 0n) {
            s += x % 10n;
            x /= 10n;
        }
        global_res = s;
    }
    return n * 19;
}

/* a xorshift generator of 64 bits: shifts, exclusive ors and
   BigInt.asUintN */
function bigint64_xorshift(n)
{
    var i, x = 88172645463325252n;
    for (i = 0; i < n; i++) {
        x ^= BigInt.asUintN(64, x << 13n);
        x ^= x >> 7n;
        x ^= BigInt.asUintN(64, x << 17n);
    }
    global_res = x;
    return n;
}

/* the 64-bit FNV-1a hash of a string, a character at a time */
function bigint64_fnv1a(n)
{
    var text = "The quick brown fox jumps over the lazy dog, again and again.";
    var i, j, h, len = text.length;
    for (j = 0; j < n; j++) {
        h = 0xcbf29ce484222325n;
        for (i = 0; i < len; i++)
            h = BigInt.asUintN(64, (h ^ BigInt(text.charCodeAt(i))) * 0x100000001b3n);
        global_res = h;
    }
    return n * len;
}

/* a binary search of a sorted array of BigInts: their comparisons */
function bigint64_search(n)
{
    var a = [], i, j, lo, hi, mid, k, c = 0, len = 1024;
    for (i = 0; i < len; i++)
        a[i] = BigInt(i) * 7919n;
    for (j = 0; j < n; j++) {
        k = BigInt(j & (len - 1)) * 7919n;
        lo = 0;
        hi = len;
        while (lo < hi) {
            mid = (lo + hi) >> 1;
            if (a[mid] < k)
                lo = mid + 1;
            else
                hi = mid;
        }
        c += lo;
    }
    global_res = c;
    return n * 10;
}

/* powers in the int64 range */
function bigint64_pow(n)
{
    var i, s = 0n;
    for (i = 0; i < n; i++)
        s += BigInt(i & 1023) ** 5n;
    global_res = s;
    return n;
}

/* microbench.js's BigInt arithmetic, of numbers of 1024 bits */
function bigint1024_arith(n)
{
    return bigint_arith(n, 1024);
}

/* modular exponentiation, as RSA makes it, by a 64-bit exponent modulo the
   Mersenne prime of 521 bits: a product and a remainder of numbers of
   that size each step */
function bigint_modpow(n)
{
    var m = (1n << 521n) - 1n, j, b, e, r, ops = 0;
    for (j = 0; j < n; j++) {
        r = 1n;
        b = (m >> 3n) + BigInt(j);
        for (e = 0xc0ffee1234567891n; e > 0n; e >>= 1n) {
            if (e & 1n) {
                r = r * b % m;
                ops++;
            }
            b = b * b % m;
            ops++;
        }
        global_res = r;
    }
    return ops;
}

/* the first hundred digits of pi by a spigot (Gibbons), its numbers
   growing as it goes */
function bigint_pidigits(n)
{
    var j, k, q, r, t, i, u, y, s;
    for (j = 0; j < n; j++) {
        q = 1n; r = 180n; t = 60n; i = 2n; s = "";
        for (k = 0; k < 100; k++) {
            u = 3n * (3n * i + 1n) * (3n * i + 2n);
            y = (q * (27n * i - 12n) + 5n * r) / (5n * t);
            s += y;
            r = 10n * u * (q * (5n * i - 2n) + r - y * t);
            q = 10n * q * i * (2n * i - 1n);
            t = t * u;
            i += 1n;
        }
        global_res = s;
    }
    return n * 100;
}

/* decimal strings of numbers of 256 bits */
function bigint_toString(n)
{
    var x = (1n << 255n) + 12345678901234567890n, j;
    for (j = 0; j < n; j++)
        global_res = (x + BigInt(j)).toString();
    return n;
}

/* validation: an e-mail pattern, anchored, tested against addresses good
   and bad */
function regexp_test(n)
{
    var j, c = 0, a = ["ada@example.com", "grace.hopper@navy.mil", "not an address",
        "x@y", "linus+kernel@kernel.org", "@missing.user", "ken@bell-labs.com", "a.b@c.d.e"];
    for (j = 0; j < n; j++)
        if (/^[\w.+-]+@[\w-]+\.[\w.-]+$/.test(a[j & 7]))
            c++;
    global_res = c;
    return n;
}

/* a tokenizer: a global pattern of alternatives with groups, exec in a
   loop until it finds nothing; an operation is a call of exec */
function regexp_exec_g(n)
{
    var j, m, c = 0, s = "for (var i = 0; i < 100; i += 2) total = total + f(i, 3.5);";
    var re = /(\d+(?:\.\d+)?)|([A-Za-z_]\w*)|(\S)/g;
    for (j = 0; j < n; j++) {
        while ((m = re.exec(s)) !== null)
            if (m[2] !== undefined)
                c++;
    }
    global_res = c;
    return n * 28;
}

/* a lexer: a sticky pattern, lastIndex carried from token to token; an
   operation is a call of exec */
function regexp_sticky(n)
{
    var j, m, c = 0, s = "let x = 10 * (y + 42) - z / 7;";
    var re = /\s*(?:(\d+)|(\w+)|([^\s\w]))/y;
    for (j = 0; j < n; j++) {
        re.lastIndex = 0;
        while ((m = re.exec(s)) !== null)
            c += m[0].length;
    }
    global_res = c;
    return n * 16;
}

/* a global replace by a template of groups */
function regexp_replace_dollar(n)
{
    var j, r, s = "2024-01-15, 2025-12-31 and 1999-07-04";
    for (j = 0; j < n; j++)
        r = s.replace(/(\d+)-(\d+)-(\d+)/g, "$3/$2/$1");
    global_res = r;
    return n;
}

/* a global replace by a function: each word capitalized */
function regexp_replace_fn(n)
{
    var j, r, s = "hello world, this is some text to capitalize";
    for (j = 0; j < n; j++)
        r = s.replace(/\b\w/g, function (c) { return c.toUpperCase(); });
    global_res = r;
    return n;
}

/* a split on a pattern */
function regexp_split(n)
{
    var j, r, s = "alpha, beta ,gamma,delta  ,  epsilon,zeta";
    for (j = 0; j < n; j++)
        r = s.split(/\s*,\s*/);
    global_res = r;
    return n;
}

/* match with a global pattern: every number in a string */
function regexp_match_g(n)
{
    var j, r, s = "x=12 y=345 z=6 w=7890 v=1";
    for (j = 0; j < n; j++)
        r = s.match(/\d+/g);
    global_res = r;
    return n;
}

/* exec of a pattern of named groups, one of them read */
function regexp_named(n)
{
    var j, m, c = 0, a = ["2024-01-15", "1999-12-31", "2000-02-29", "1970-01-01"];
    for (j = 0; j < n; j++) {
        m = /(?<year>\d{4})-(?<month>\d{2})-(?<day>\d{2})/.exec(a[j & 3]);
        c += m.groups.month.length;
    }
    global_res = c;
    return n;
}

/* a case-insensitive pattern with the u flag, over text not all ASCII */
function regexp_unicode(n)
{
    var j, c = 0, a = ["Ärger über Öl", "naïve café", "ΣΊΣΥΦΟΣ", "plain ascii"];
    for (j = 0; j < n; j++)
        if (/über|café|σίσυφος/iu.test(a[j & 3]))
            c++;
    global_res = c;
    return n;
}

/* a class of a Unicode property: whether a text of several scripts is
   letters, spaces and punctuation alone */
function regexp_property(n)
{
    var j, c = 0, a = ["Ärger über Öl und Äpfel, naïve Grüße aus Köln",
        "Ἀθῆναι καὶ Σπάρτη, Ὅμηρος καὶ Ἡσίοδος",
        "Москва и Санкт-Петербург, Достоевский и Толстой",
        "東京と大阪、京都と奈良、富士山と琵琶湖"];
    for (j = 0; j < n; j++)
        if (/^[\p{L}\s,.、-]+$/u.test(a[j & 3]))
            c++;
    global_res = c;
    return n;
}

/* a case-insensitive search through a text of some kilobytes */
function regexp_search_i(n)
{
    var j, c = 0, s = "";
    for (j = 0; j < 40; j++)
        s += "The quick brown fox jumps over the lazy dog. ";
    s += "Needle";
    for (j = 0; j < n; j++)
        if (/needle/i.test(s))
            c++;
    global_res = c;
    return n;
}

/* a search for a word through a text of some kilobytes not all ASCII */
function regexp_search_utf16(n)
{
    var j, c = 0, s = "";
    for (j = 0; j < 40; j++)
        s += "Die Größe der Straße überrascht. ";
    s += "needle";
    for (j = 0; j < n; j++)
        if (/needle/.test(s))
            c++;
    global_res = c;
    return n;
}

/* a pattern tested against a string made just before */
function regexp_fresh(n)
{
    var j, c = 0;
    for (j = 0; j < n; j++)
        if (/^item-\d+[05]$/.test("item-" + j))
            c++;
    global_res = c;
    return n;
}

/* objects as Are We Fast Yet's are written: a list of elements of two
   properties, walked by a method while each next is not null */
class ExtraElement
{
    constructor(v) { this.val = v; this.next = null; }
}

class ExtraList
{
    constructor(n)
    {
        var i;
        this.head = null;
        for (i = 0; i < n; i++) {
            const e = new ExtraElement(i);
            e.next = this.head;
            this.head = e;
        }
    }
    count()
    {
        let x = this.head, k = 0;
        while (x !== null) {
            k += 1;
            x = x.next;
        }
        return k;
    }
}

function list_walk(n)
{
    var j, l = new ExtraList(32), s = 0;
    for (j = 0; j < n; j += 32)
        s = (s + l.count()) | 0;
    global_res = s;
    return n;
}

/* small methods of an object keeping its state in an array, calling each
   other with their parameters, as Towers' piles are moved */
class ExtraPiles
{
    constructor()
    {
        this.piles = [null, null, null];
        this.moves = 0;
    }
    push(d, p)
    {
        const top = this.piles[p];
        if (top && d.size >= top.size)
            throw new Error("too big");
        d.next = top;
        this.piles[p] = d;
    }
    pop(p)
    {
        const top = this.piles[p];
        if (top === null)
            throw new Error("empty");
        this.piles[p] = top.next;
        top.next = null;
        return top;
    }
    move(from, to)
    {
        this.push(this.pop(from), to);
        this.moves += 1;
    }
}

function method_moves(n)
{
    var j, o = new ExtraPiles();
    o.push({ size: 1, next: null }, 0);
    for (j = 0; j < n; j += 2) {
        o.move(0, 1);
        o.move(1, 0);
    }
    global_res = o.moves;
    return n;
}

/* a class's elements walked by forEach with an arrow ending in a method
   call, as DeltaBlue executes a plan */
class ExtraVector
{
    constructor() { this.storage = []; this.size = 0; }
    append(e) { this.storage[this.size] = e; this.size += 1; }
    forEach(fn)
    {
        for (let i = 0; i < this.size; i += 1)
            fn(this.storage[i]);
    }
}

class ExtraStep
{
    constructor(a, b) { this.from = a; this.to = b; this.dir = "forward"; }
    run()
    {
        if (this.dir === "forward")
            this.to.v = this.from.v;
        else
            this.from.v = this.to.v;
    }
}

function method_forEach(n)
{
    var j, plan = new ExtraVector(), prev = { v: 1 };
    for (j = 0; j < 64; j++) {
        const next = { v: 0 };
        plan.append(new ExtraStep(prev, next));
        prev = next;
    }
    for (j = 0; j < n; j += 64)
        plan.forEach((c) => c.run());
    global_res = prev.v;
    return n;
}

/* a method making an arrow of its parameters and this at each call, for
   forEach, as Havlak's do */
class ExtraPool
{
    constructor() { this.items = new ExtraVector(); this.n = 0; for (let i = 0; i < 4; i++) this.items.append(i); }
    step(w, pool)
    {
        this.items.forEach((x) => {
            if (x !== w) {
                pool.append(x);
                this.n += 1;
            }
        });
    }
}

function method_closure(n)
{
    var j, h = new ExtraPool();
    for (j = 0; j < n; j++)
        h.step(j & 3, new ExtraVector());
    global_res = h.n;
    return n;
}

/* an object made on a prototype of its own each time, as code that makes
   one-off objects with Object.create does: the prototype dropped with it */
function object_create_fresh(n)
{
    var j, s = 0, o;
    for (j = 0; j < n; j++) {
        o = Object.create({ x: j });
        o.y = 1;
        s += o.x + o.y;
    }
    global_res = s;
    return n;
}

/* objects made on one prototype they share, the common case beside it */
function object_create_shared(n)
{
    var proto = { x: 1 }, j, s = 0, o;
    for (j = 0; j < n; j++) {
        o = Object.create(proto);
        o.y = j;
        s += o.x + o.y;
    }
    global_res = s;
    return n;
}

/* a literal whose prototype is an object made with it: { __proto__: p } */
function proto_literal_fresh(n)
{
    var j, s = 0, o;
    for (j = 0; j < n; j++) {
        o = { __proto__: { z: j }, w: 1 };
        s += o.z + o.w;
    }
    global_res = s;
    return n;
}

/* an object given a prototype of its own after it is made */
function set_proto_fresh(n)
{
    var j, s = 0, o;
    for (j = 0; j < n; j++) {
        o = Object.setPrototypeOf({}, { k: j });
        o.v = 1;
        s += o.k + o.v;
    }
    global_res = s;
    return n;
}

/* a class made by each call, as a factory of mixins makes one, and an
   instance of it */
function class_factory(n)
{
    var j, s = 0, C, o;
    for (j = 0; j < n; j++) {
        C = class { constructor(a) { this.a = a; } get() { return this.a; } };
        o = new C(j);
        s += o.get();
    }
    global_res = s;
    return n;
}

/* a subclass of one base made by each call, and an instance of it */
function subclass_fresh(n)
{
    class Base { constructor(a) { this.a = a; } }
    var j, s = 0, D, o;
    for (j = 0; j < n; j++) {
        D = class extends Base { constructor(a) { super(a); this.b = 1; } };
        o = new D(j);
        s += o.a + o.b;
    }
    global_res = s;
    return n;
}

/* scopes made as objects, each on the one it encloses, as an interpreter
   written in JavaScript makes them: a frame on a fresh parent, and one on
   the frame */
function scope_chain(n)
{
    var j, s = 0, a, b;
    for (j = 0; j < n; j++) {
        a = Object.create({ base: j });
        a.i = j;
        b = Object.create(a);
        b.v = 1;
        s += b.base + b.i + b.v;
    }
    global_res = s;
    return n;
}

/* one constructor made for alternating new.targets, as Babel's
   _createSuper makes subclasses' instances: Reflect.construct(Base, args,
   NewTarget) */
function construct_new_target(n)
{
    function A(x) { this.x = x; this.y = 1; }
    function B() {}
    function C() {}
    B.prototype = Object.create(A.prototype);
    C.prototype = Object.create(A.prototype);
    var j, s = 0, o, args = [0];
    for (j = 0; j < n; j++) {
        args[0] = j;
        o = Reflect.construct(A, args, (j & 1) ? B : C);
        s += o.x + o.y;
    }
    global_res = s;
    return n;
}

/* Objects no code outside a function sees but at a join, round a loop or
   on a rare path: what partial escape analysis leaves unmade. */
function ExtraVec2(x, y)
{
    this.x = x;
    this.y = y;
}
ExtraVec2.prototype.add = function (o) { return new ExtraVec2(this.x + o.x, this.y + o.y); };

var extra_vecs = [];
for (var extra_i = 0; extra_i < 100; extra_i++)
    extra_vecs.push(new ExtraVec2(extra_i, extra_i & 7));

function extra_vec_sum(a, m)
{
    var i, s = new ExtraVec2(0, 0);
    for (i = 0; i < m; i++)
        s = s.add(a[i % a.length]);
    return s.x + s.y;
}

/* an accumulator a loop replaces each turn through a method, sum =
   sum.add(v), 100 turns a call */
function escape_accumulate(n)
{
    var j, r = 0;
    for (j = 0; j < n; j++)
        r += extra_vec_sum(extra_vecs, 100);
    global_res = r;
    return n * 100;
}

/* the same, one long call: the first run reaches the loop in the
   interpreter, where native code is entered at the loop */
function escape_accumulate_long(n)
{
    global_res = extra_vec_sum(extra_vecs, n * 100);
    return n * 100;
}

/* objects made on either branch and merged at the join */
function escape_join(n)
{
    var j, p, s = 0;
    for (j = 0; j < n; j++) {
        if (j & 1)
            p = new ExtraVec2(j, 1);
        else
            p = new ExtraVec2(1, j & 15);
        s += p.x * p.y;
    }
    global_res = s;
    return n;
}

/* an object that leaves the function on one turn in 1024 */
function escape_rare(n)
{
    var j, o, s = 0, kept = null;
    for (j = 0; j < n; j++) {
        o = { k: j, v: j & 7 };
        if ((j & 1023) == 0)
            kept = o;
        s += o.k + o.v;
    }
    global_res = s + kept.k;
    return n;
}

/* the same keys from eight literals, read at one site */
var extra_lit_same = [
    function (j) { return { x: j, y: 1 }; }, function (j) { return { x: j, y: 2 }; },
    function (j) { return { x: j, y: 3 }; }, function (j) { return { x: j, y: 4 }; },
    function (j) { return { x: j, y: 5 }; }, function (j) { return { x: j, y: 6 }; },
    function (j) { return { x: j, y: 7 }; }, function (j) { return { x: j, y: 8 }; }
];

function extra_lit_x(o)
{
    return o.x;
}

function literal_sites_read(n)
{
    var j, s = 0;
    for (j = 0; j < n; j++)
        s += extra_lit_x(extra_lit_same[j & 7](j));
    global_res = s;
    return n;
}

/* literals of one first key and eight second ones */
var extra_lit_branch = [
    function (j) { return { x: j, a: 1 }; }, function (j) { return { x: j, b: 1 }; },
    function (j) { return { x: j, c: 1 }; }, function (j) { return { x: j, d: 1 }; },
    function (j) { return { x: j, e: 1 }; }, function (j) { return { x: j, f: 1 }; },
    function (j) { return { x: j, g: 1 }; }, function (j) { return { x: j, h: 1 }; }
];

function literal_sites_branch(n)
{
    var j, s = 0;
    for (j = 0; j < n; j++)
        s += extra_lit_x(extra_lit_branch[j & 7](j));
    global_res = s;
    return n;
}

/* the same, each value a call, so that each key is defined by itself */
function extra_lit_id(v)
{
    return v;
}

var extra_lit_spill = [
    function (j) { return { x: extra_lit_id(j), a: extra_lit_id(1) }; },
    function (j) { return { x: extra_lit_id(j), b: extra_lit_id(1) }; },
    function (j) { return { x: extra_lit_id(j), c: extra_lit_id(1) }; },
    function (j) { return { x: extra_lit_id(j), d: extra_lit_id(1) }; }
];

function literal_sites_spill(n)
{
    var j, s = 0;
    for (j = 0; j < n; j++)
        s += extra_lit_x(extra_lit_spill[j & 3](j));
    global_res = s;
    return n;
}

/* Objects a loop carries from turn to turn, each turn's replacing the
   last, which an engine may keep out of the heap where nothing else sees
   them. carried_short and carried_long are what that is for; the rest are
   what it may cost where it does not pay: functions that run once, a loop
   left early, a loop left on most turns, shapes that keep changing. */

function ExtraVec(x, y)
{
    this.x = x;
    this.y = y;
}

ExtraVec.prototype.add = function (o)
{
    return new ExtraVec(this.x + o.x, this.y + o.y);
};

/* where a function made by new Function finds it, whether this file runs as
   a script or as node's module */
globalThis.ExtraVec = ExtraVec;

var extra_carried_obj = { a: 1 };
var extra_carried_id = 0;

function extra_carried_short(turns)
{
    let s = new ExtraVec(0, 0);
    const d = new ExtraVec(1, 2);
    for (let i = 0; i < turns; i++)
        s = s.add(d);
    return s.x + s.y;
}

/* short loops, nothing leaving them */
function carried_short(n)
{
    var j, t = 0;
    for (j = 0; j < n; j++)
        t += extra_carried_short(100);
    global_res = t;
    return n * 100;
}

function extra_carried_long(turns)
{
    let s = new ExtraVec(0, 0);
    const d = new ExtraVec(1, 2);
    for (let i = 0; i < turns; i++)
        s = s.add(d);
    return s.x + s.y;
}

/* long loops, the first of them a frame that has to enter compiled code at
   its loop */
function carried_long(n)
{
    var j, t = 0;
    for (j = 0; j < n; j++)
        t += extra_carried_long(20000);
    global_res = t;
    return n * 20000;
}

/* a function of its own, compiled anew, called once, for turns turns */
function extra_carried_fresh(turns)
{
    var id = extra_carried_id++;
    var f = new Function("n",
                         "let s = new ExtraVec(" + id + ", 0); const d = new ExtraVec(1, 2);" +
                         "for (let i = 0; i < n; i++) s = s.add(d);" +
                         "return s.x + s.y;");
    return f(turns) - id;
}

/* functions each called once, for long enough to be worth compiling */
function carried_fresh(n)
{
    var j, t = 0;
    for (j = 0; j < n; j++)
        t += extra_carried_fresh(20000);
    global_res = t;
    return n * 20000;
}

/* functions each called once, for long */
function carried_fresh_long(n)
{
    var j, t = 0;
    for (j = 0; j < n; j++)
        t += extra_carried_fresh(2000000);
    global_res = t;
    return n * 2000000;
}

/* a loop each call leaves native code in at its sixth turn, at a write of
   a number's property, as long as each name says: all its turns but five
   past that */
function extra_carried_exit_3000(n, k)
{
    let s = new ExtraVec(0, 0);
    const d = new ExtraVec(1, 2);
    for (let i = 0; i < n; i++) {
        const o = i === k ? 1 : extra_carried_obj;
        o.a = 1;
        s = s.add(d);
    }
    return s.x + s.y;
}

function carried_exit_3000(n)
{
    var j, t = 0;
    for (j = 0; j < n; j++)
        t += extra_carried_exit_3000(3000, 5);
    global_res = t;
    return n * 3000;
}

function extra_carried_exit_500(n, k)
{
    let s = new ExtraVec(0, 0);
    const d = new ExtraVec(1, 2);
    for (let i = 0; i < n; i++) {
        const o = i === k ? 1 : extra_carried_obj;
        o.a = 1;
        s = s.add(d);
    }
    return s.x + s.y;
}

function carried_exit_500(n)
{
    var j, t = 0;
    for (j = 0; j < n; j++)
        t += extra_carried_exit_500(500, 5);
    global_res = t;
    return n * 500;
}

function extra_carried_exit_100(n, k)
{
    let s = new ExtraVec(0, 0);
    const d = new ExtraVec(1, 2);
    for (let i = 0; i < n; i++) {
        const o = i === k ? 1 : extra_carried_obj;
        o.a = 1;
        s = s.add(d);
    }
    return s.x + s.y;
}

function carried_exit_100(n)
{
    var j, t = 0;
    for (j = 0; j < n; j++)
        t += extra_carried_exit_100(100, 5);
    global_res = t;
    return n * 100;
}

function extra_carried_exit_20(n, k)
{
    let s = new ExtraVec(0, 0);
    const d = new ExtraVec(1, 2);
    for (let i = 0; i < n; i++) {
        const o = i === k ? 1 : extra_carried_obj;
        o.a = 1;
        s = s.add(d);
    }
    return s.x + s.y;
}

function carried_exit_20(n)
{
    var j, t = 0;
    for (j = 0; j < n; j++)
        t += extra_carried_exit_20(20, 5);
    global_res = t;
    return n * 20;
}

function extra_carried_exit_7(n, k)
{
    let s = new ExtraVec(0, 0);
    const d = new ExtraVec(1, 2);
    for (let i = 0; i < n; i++) {
        const o = i === k ? 1 : extra_carried_obj;
        o.a = 1;
        s = s.add(d);
    }
    return s.x + s.y;
}

function carried_exit_7(n)
{
    var j, t = 0;
    for (j = 0; j < n; j++)
        t += extra_carried_exit_7(7, 5);
    global_res = t;
    return n * 7;
}

function extra_carried_mixed(n, k)
{
    let s = new ExtraVec(0, 0);
    const d = new ExtraVec(1, 2);
    for (let i = 0; i < n; i++) {
        const o = i === k ? 1 : extra_carried_obj;
        o.a = 1;
        s = s.add(d);
    }
    return s.x + s.y;
}

/* every other call leaves, the others run their loop through */
function carried_exit_mixed(n)
{
    var j, t = 0;
    for (j = 0; j < n; j++)
        t += extra_carried_mixed(500, j % 2 === 0 ? 5 : -1);
    global_res = t;
    return n * 500;
}

function extra_carried_loops(n, k)
{
    const d = new ExtraVec(1, 2);
    let t = 0;
    { let s = new ExtraVec(0, 0); for (let i = 0; i < n; i++) { const o = i === k ? 1 : extra_carried_obj; o.a = 1; s = s.add(d); } t += s.x; }
    { let s = new ExtraVec(1, 0); for (let i = 0; i < n; i++) { const o = i === k ? 1 : extra_carried_obj; o.a = 1; s = s.add(d); } t += s.x; }
    { let s = new ExtraVec(2, 0); for (let i = 0; i < n; i++) { const o = i === k ? 1 : extra_carried_obj; o.a = 1; s = s.add(d); } t += s.x; }
    { let s = new ExtraVec(3, 0); for (let i = 0; i < n; i++) { const o = i === k ? 1 : extra_carried_obj; o.a = 1; s = s.add(d); } t += s.x; }
    { let s = new ExtraVec(4, 0); for (let i = 0; i < n; i++) { const o = i === k ? 1 : extra_carried_obj; o.a = 1; s = s.add(d); } t += s.x; }
    { let s = new ExtraVec(5, 0); for (let i = 0; i < n; i++) { const o = i === k ? 1 : extra_carried_obj; o.a = 1; s = s.add(d); } t += s.x; }
    { let s = new ExtraVec(6, 0); for (let i = 0; i < n; i++) { const o = i === k ? 1 : extra_carried_obj; o.a = 1; s = s.add(d); } t += s.x; }
    { let s = new ExtraVec(7, 0); for (let i = 0; i < n; i++) { const o = i === k ? 1 : extra_carried_obj; o.a = 1; s = s.add(d); } t += s.x; }
    { let s = new ExtraVec(8, 0); for (let i = 0; i < n; i++) { const o = i === k ? 1 : extra_carried_obj; o.a = 1; s = s.add(d); } t += s.x; }
    { let s = new ExtraVec(9, 0); for (let i = 0; i < n; i++) { const o = i === k ? 1 : extra_carried_obj; o.a = 1; s = s.add(d); } t += s.x; }
    { let s = new ExtraVec(10, 0); for (let i = 0; i < n; i++) { const o = i === k ? 1 : extra_carried_obj; o.a = 1; s = s.add(d); } t += s.x; }
    { let s = new ExtraVec(11, 0); for (let i = 0; i < n; i++) { const o = i === k ? 1 : extra_carried_obj; o.a = 1; s = s.add(d); } t += s.x; }
    { let s = new ExtraVec(12, 0); for (let i = 0; i < n; i++) { const o = i === k ? 1 : extra_carried_obj; o.a = 1; s = s.add(d); } t += s.x; }
    { let s = new ExtraVec(13, 0); for (let i = 0; i < n; i++) { const o = i === k ? 1 : extra_carried_obj; o.a = 1; s = s.add(d); } t += s.x; }
    { let s = new ExtraVec(14, 0); for (let i = 0; i < n; i++) { const o = i === k ? 1 : extra_carried_obj; o.a = 1; s = s.add(d); } t += s.x; }
    { let s = new ExtraVec(15, 0); for (let i = 0; i < n; i++) { const o = i === k ? 1 : extra_carried_obj; o.a = 1; s = s.add(d); } t += s.x; }
    { let s = new ExtraVec(16, 0); for (let i = 0; i < n; i++) { const o = i === k ? 1 : extra_carried_obj; o.a = 1; s = s.add(d); } t += s.x; }
    { let s = new ExtraVec(17, 0); for (let i = 0; i < n; i++) { const o = i === k ? 1 : extra_carried_obj; o.a = 1; s = s.add(d); } t += s.x; }
    { let s = new ExtraVec(18, 0); for (let i = 0; i < n; i++) { const o = i === k ? 1 : extra_carried_obj; o.a = 1; s = s.add(d); } t += s.x; }
    { let s = new ExtraVec(19, 0); for (let i = 0; i < n; i++) { const o = i === k ? 1 : extra_carried_obj; o.a = 1; s = s.add(d); } t += s.x; }
    { let s = new ExtraVec(20, 0); for (let i = 0; i < n; i++) { const o = i === k ? 1 : extra_carried_obj; o.a = 1; s = s.add(d); } t += s.x; }
    { let s = new ExtraVec(21, 0); for (let i = 0; i < n; i++) { const o = i === k ? 1 : extra_carried_obj; o.a = 1; s = s.add(d); } t += s.x; }
    { let s = new ExtraVec(22, 0); for (let i = 0; i < n; i++) { const o = i === k ? 1 : extra_carried_obj; o.a = 1; s = s.add(d); } t += s.x; }
    { let s = new ExtraVec(23, 0); for (let i = 0; i < n; i++) { const o = i === k ? 1 : extra_carried_obj; o.a = 1; s = s.add(d); } t += s.x; }
    return t;
}

/* a function of two dozen such loops, each left early */
function carried_exit_loops(n)
{
    var j, t = 0;
    for (j = 0; j < n; j++)
        t += extra_carried_loops(200, 5);
    global_res = t;
    return n * 200 * 24;
}

/* left on one turn of each two and of each eight, an object live at each */
function extra_carried_every_2(turns)
{
    let s = new ExtraVec(0, 0);
    const d = new ExtraVec(1, 2);
    for (let i = 0; i < turns; i++) {
        const o = i % 2 === 0 ? 1 : extra_carried_obj;
        o.a = 1;
        s = s.add(d);
    }
    return s.x + s.y;
}

function carried_every_2(n)
{
    var j, t = 0;
    for (j = 0; j < n; j++)
        t += extra_carried_every_2(1000);
    global_res = t;
    return n * 1000;
}

function extra_carried_every_8(turns)
{
    let s = new ExtraVec(0, 0);
    const d = new ExtraVec(1, 2);
    for (let i = 0; i < turns; i++) {
        const o = i % 8 === 0 ? 1 : extra_carried_obj;
        o.a = 1;
        s = s.add(d);
    }
    return s.x + s.y;
}

function carried_every_8(n)
{
    var j, t = 0;
    for (j = 0; j < n; j++)
        t += extra_carried_every_8(1000);
    global_res = t;
    return n * 1000;
}

function extra_carried_phase(o, n)
{
    let s = new ExtraVec(0, 0);
    for (let i = 0; i < n; i++)
        s = new ExtraVec(s.x + o.k, s.y + 1);
    return s.x + s.y;
}

/* a loop reading objects of a new shape each round, its code compiled
   again for them */
function carried_phases(n)
{
    var j, k, o, t = 0;
    for (j = 0; j < n; j++) {
        o = {};
        for (k = 0; k <= j % 12; k++)
            o["p" + k] = k;
        o.k = j;
        t += extra_carried_phase(o, 5000);
    }
    global_res = t;
    return n * 5000;
}

var extra_carried_box = [];

function extra_carried_stored(turns)
{
    let s = new ExtraVec(0, 0);
    const d = new ExtraVec(1, 2);
    for (let i = 0; i < turns; i++) {
        s = s.add(d);
        extra_carried_box[0] = s;
    }
    return s.x;
}

/* each turn's object stored where it is seen: none can stay out of the
   heap */
function carried_stored(n)
{
    var j, t = 0;
    for (j = 0; j < n; j++)
        t += extra_carried_stored(1000);
    global_res = t;
    return n * 1000;
}

/* JSON.parse and JSON.stringify. Each document is made once, deterministically,
   and a test's operation is an element of it: a record, a number, a string.
   The first are what APIs send, an array of objects of one layout; the rest
   are what a fast path can lose on: floats that take every digit, objects
   used as dictionaries, layouts that change from one element to the next,
   keys that are indices, a replacer or a toJSON for each value. */

function extra_json_record(i)
{
    return { id: i, name: "item " + i, price: i * 1.25, active: (i & 1) == 0,
             tags: ["a", "bb", "ccc"],
             size: { width: i % 100, height: 20.5, depth: null } };
}

function extra_json_list(m, f)
{
    var i, a = [];
    for (i = 0; i < m; i++)
        a.push(f(i));
    return a;
}

var extra_json_records = extra_json_list(100, extra_json_record);
var extra_json_records_text = JSON.stringify(extra_json_records);

var extra_json_small = { id: 7, name: "x", ok: true, list: [1, 2, 3] };
var extra_json_small_text = JSON.stringify(extra_json_small);

var extra_json_ints = extra_json_list(1000, function (i) { return i * 37; });
var extra_json_ints_text = JSON.stringify(extra_json_ints);

/* numbers of a few digits, as prices and coordinates are */
var extra_json_decimals = extra_json_list(1000, function (i) { return i * 0.25 + 0.5; });
var extra_json_decimals_text = JSON.stringify(extra_json_decimals);

/* doubles that take all of their 17 digits */
var extra_json_floats = extra_json_list(1000, function (i) { return Math.sqrt(i + 2) * 1e-3; });
var extra_json_floats_text = JSON.stringify(extra_json_floats);

var extra_json_strings = extra_json_list(1000, function (i) { return "a plain string " + i; });
var extra_json_strings_text = JSON.stringify(extra_json_strings);

/* strings where a third of the characters want an escape */
var extra_json_escapes = extra_json_list(1000, function (i) { return "a\n\"b\"\tc\\" + i; });
var extra_json_escapes_text = JSON.stringify(extra_json_escapes);

var extra_json_unicode = extra_json_list(1000, function (i) {
    return String.fromCharCode(0x4e2d, 0x6587, 0x5b57, 0x7b26, 0x20, 0xe9, 0x20) + i;
});
var extra_json_unicode_text = JSON.stringify(extra_json_unicode);

var extra_json_long = (function () {
    var i, s = "";
    for (i = 0; i < 6554; i++)
        s += "ten chars.";
    return s;
})();
var extra_json_long_text = JSON.stringify(extra_json_long);

/* one object of 1000 keys no other object has */
var extra_json_dict = (function () {
    var i, o = {};
    for (i = 0; i < 1000; i++)
        o["key" + (i * 7919)] = i;
    return o;
})();
var extra_json_dict_text = JSON.stringify(extra_json_dict);

/* many small objects, each with keys of its own */
var extra_json_unique = extra_json_list(100, function (i) {
    var o = {};
    o["a" + i] = 1;
    o["b" + i] = 2;
    o["c" + i] = 3;
    return o;
});
var extra_json_unique_text = JSON.stringify(extra_json_unique);

/* objects of four layouts in turn, so that no object is laid out as the
   one before it */
var extra_json_varied = extra_json_list(100, function (i) {
    switch (i & 3) {
    case 0: return { id: i, name: "x" };
    case 1: return { name: "x", id: i };
    case 2: return { id: i, name: "x", extra: true };
    default: return { kind: 1, id: i, name: "x" };
    }
});
var extra_json_varied_text = JSON.stringify(extra_json_varied);

/* objects laid out alike but for the key of their last property */
var extra_json_last_differs = extra_json_list(100, function (i) {
    var o = { id: i, name: "x", active: true };
    o["k" + (i & 7)] = i;
    return o;
});
var extra_json_last_differs_text = JSON.stringify(extra_json_last_differs);

var extra_json_index_keys = extra_json_list(100, function (i) {
    return { 3: i, 1: "x", 2: true };
});
var extra_json_index_keys_text = JSON.stringify(extra_json_index_keys);

var extra_json_deep = (function () {
    var i, v = 1;
    for (i = 0; i < 64; i++)
        v = i & 1 ? [v] : { v: v };
    return v;
})();
var extra_json_deep_text = JSON.stringify(extra_json_deep);

function ExtraJSONPoint(x, y)
{
    this.x = x;
    this.y = y;
}
ExtraJSONPoint.prototype.norm = function () { return this.x + this.y; };

/* instances of a class, whose methods stringify walks past */
var extra_json_instances = extra_json_list(100, function (i) {
    return new ExtraJSONPoint(i, i + 1);
});

function ExtraJSONStamp(t)
{
    this.t = t;
}
ExtraJSONStamp.prototype.toJSON = function () { return this.t; };

var extra_json_stamps = extra_json_list(100, function (i) {
    return new ExtraJSONStamp(i);
});

var extra_json_getters = extra_json_list(100, function (i) {
    return { get id() { return i; }, name: "x" };
});

function extra_json_parse_test(n, text, each)
{
    var j, v;
    for (j = 0; j < n; j++)
        v = JSON.parse(text);
    global_res = v;
    return n * each;
}

function extra_json_stringify_test(n, v, each)
{
    var j, s;
    for (j = 0; j < n; j++)
        s = JSON.stringify(v);
    global_res = s;
    return n * each;
}

function json_parse_records(n) { return extra_json_parse_test(n, extra_json_records_text, 100); }
function json_parse_small(n) { return extra_json_parse_test(n, extra_json_small_text, 1); }
function json_parse_ints(n) { return extra_json_parse_test(n, extra_json_ints_text, 1000); }
function json_parse_decimals(n) { return extra_json_parse_test(n, extra_json_decimals_text, 1000); }
function json_parse_floats(n) { return extra_json_parse_test(n, extra_json_floats_text, 1000); }
function json_parse_strings(n) { return extra_json_parse_test(n, extra_json_strings_text, 1000); }
function json_parse_escapes(n) { return extra_json_parse_test(n, extra_json_escapes_text, 1000); }
function json_parse_unicode(n) { return extra_json_parse_test(n, extra_json_unicode_text, 1000); }
function json_parse_long(n) { return extra_json_parse_test(n, extra_json_long_text, 1); }
function json_parse_dict(n) { return extra_json_parse_test(n, extra_json_dict_text, 1000); }
function json_parse_unique(n) { return extra_json_parse_test(n, extra_json_unique_text, 100); }
function json_parse_varied(n) { return extra_json_parse_test(n, extra_json_varied_text, 100); }
function json_parse_last_differs(n) { return extra_json_parse_test(n, extra_json_last_differs_text, 100); }
function json_parse_index_keys(n) { return extra_json_parse_test(n, extra_json_index_keys_text, 100); }
function json_parse_deep(n) { return extra_json_parse_test(n, extra_json_deep_text, 64); }

/* a key given twice, of which the last wins */
function json_parse_duplicates(n)
{
    return extra_json_parse_test(n, '{"a":1,"b":2,"a":3,"c":4,"b":5}', 1);
}

/* a reviver's call for each value of a record */
function json_parse_reviver(n)
{
    var j, v;
    function keep(k, v) { return v; }
    for (j = 0; j < n; j++)
        v = JSON.parse(extra_json_records_text, keep);
    global_res = v;
    return n * 100;
}

/* each document made just before it is parsed, as one read from a socket is */
function json_parse_fresh(n)
{
    var j, v;
    for (j = 0; j < n; j++)
        v = JSON.parse('{"id":' + j + ',"name":"x"}');
    global_res = v;
    return n;
}

function json_stringify_records(n) { return extra_json_stringify_test(n, extra_json_records, 100); }
function json_stringify_small(n) { return extra_json_stringify_test(n, extra_json_small, 1); }
function json_stringify_ints(n) { return extra_json_stringify_test(n, extra_json_ints, 1000); }
function json_stringify_decimals(n) { return extra_json_stringify_test(n, extra_json_decimals, 1000); }
function json_stringify_floats(n) { return extra_json_stringify_test(n, extra_json_floats, 1000); }
function json_stringify_strings(n) { return extra_json_stringify_test(n, extra_json_strings, 1000); }
function json_stringify_escapes(n) { return extra_json_stringify_test(n, extra_json_escapes, 1000); }
function json_stringify_unicode(n) { return extra_json_stringify_test(n, extra_json_unicode, 1000); }
function json_stringify_long(n) { return extra_json_stringify_test(n, extra_json_long, 1); }
function json_stringify_dict(n) { return extra_json_stringify_test(n, extra_json_dict, 1000); }
function json_stringify_unique(n) { return extra_json_stringify_test(n, extra_json_unique, 100); }
function json_stringify_varied(n) { return extra_json_stringify_test(n, extra_json_varied, 100); }
function json_stringify_last_differs(n) { return extra_json_stringify_test(n, extra_json_last_differs, 100); }
function json_stringify_index_keys(n) { return extra_json_stringify_test(n, extra_json_index_keys, 100); }
function json_stringify_deep(n) { return extra_json_stringify_test(n, extra_json_deep, 64); }
function json_stringify_instances(n) { return extra_json_stringify_test(n, extra_json_instances, 100); }
function json_stringify_tojson(n) { return extra_json_stringify_test(n, extra_json_stamps, 100); }
function json_stringify_getters(n) { return extra_json_stringify_test(n, extra_json_getters, 100); }

/* a Date's toJSON for each element */
function json_stringify_dates(n)
{
    var j, s, a = [new Date(0), new Date(1e12)];
    for (j = 0; j < n; j++)
        s = JSON.stringify(a);
    global_res = s;
    return n * 2;
}

function json_stringify_indent(n)
{
    var j, s;
    for (j = 0; j < n; j++)
        s = JSON.stringify(extra_json_records, null, 2);
    global_res = s;
    return n * 100;
}

function json_stringify_replacer(n)
{
    var j, s;
    function keep(k, v) { return v; }
    for (j = 0; j < n; j++)
        s = JSON.stringify(extra_json_records, keep);
    global_res = s;
    return n * 100;
}

function json_stringify_allow(n)
{
    var j, s, keys = ["id", "name"];
    for (j = 0; j < n; j++)
        s = JSON.stringify(extra_json_records, keys);
    global_res = s;
    return n * 100;
}

/* a deep copy by a round trip */
function json_round_trip(n)
{
    var j, v;
    for (j = 0; j < n; j++)
        v = JSON.parse(JSON.stringify(extra_json_small));
    global_res = v;
    return n;
}

/* a document as a person reads it, with its indentation */
var extra_json_pretty_text = JSON.stringify(extra_json_records, null, 2);

function json_parse_pretty(n) { return extra_json_parse_test(n, extra_json_pretty_text, 100); }

/* numbers with an exponent, as a program in another language writes floats */
var extra_json_exponents_text = JSON.stringify(extra_json_list(1000, function (i) {
    return (i + 1) * 1.5e-9;
}));

function json_parse_exponents(n) { return extra_json_parse_test(n, extra_json_exponents_text, 1000); }

/* each object given a property after it is parsed, past the room it was made
   with */
function json_parse_add(n)
{
    var j, i, a, s = 0;
    for (j = 0; j < n; j++) {
        a = JSON.parse(extra_json_records_text);
        for (i = 0; i < a.length; i++)
            a[i].seen = true;
    }
    global_res = a;
    return n * 100;
}

/* each record read after it is parsed */
function json_parse_read(n)
{
    var j, i, a, s = 0;
    for (j = 0; j < n; j++) {
        a = JSON.parse(extra_json_records_text);
        for (i = 0; i < a.length; i++)
            s += a[i].id + a[i].size.width;
    }
    global_res = s;
    return n * 100;
}

/* a key no document had: a layout made for one object, and not met again */
function json_parse_new_layout(n)
{
    var j, v;
    for (j = 0; j < n; j++)
        v = JSON.parse('{"a":1,"k' + (j % 1000) + '":2,"x' + j + '":3}');
    global_res = v;
    return n;
}

/* a layout written once and not again */
function json_stringify_new_layout(n)
{
    var j, o, s;
    for (j = 0; j < n; j++) {
        o = { a: 1 };
        o["k" + (j % 1000)] = 2;
        o["x" + j] = 3;
        s = JSON.stringify(o);
    }
    global_res = s;
    return n;
}

/* a prototype that changes between writes, so that what was found on the
   chain has to be found again */
function json_stringify_proto_changes(n)
{
    var j, s, o = new ExtraJSONPoint(1, 2);
    for (j = 0; j < n; j++) {
        ExtraJSONPoint.prototype["m" + (j & 1)] = j;
        delete ExtraJSONPoint.prototype["m" + ((j & 1) ^ 1)];
        s = JSON.stringify(o);
    }
    global_res = s;
    return n;
}

/* a string that a loop has just joined, not yet flat */
function json_stringify_built(n)
{
    var j, i, s, r;
    for (j = 0; j < n; j++) {
        s = "";
        for (i = 0; i < 16; i++)
            s += "chunk";
        r = JSON.stringify(s);
    }
    global_res = r;
    return n;
}
