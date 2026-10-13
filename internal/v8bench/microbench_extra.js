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
