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

/* a method whose body has a loop, which it does not enter, so that it is
   called with a frame of its own, and a method that only returns its
   argument */
function ExtraPoint(x)
{
    this.x = x;
}

ExtraPoint.prototype.sum = function (k)
{
    var j, s = 0;
    for (j = 0; j < k; j++)
        s += this.x;
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
