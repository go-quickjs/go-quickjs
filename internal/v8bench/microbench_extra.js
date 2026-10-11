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
