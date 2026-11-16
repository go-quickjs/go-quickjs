var log = [];
async function a1() { log.push('a1 start'); return 1; }
async function a2() { log.push('a2 start'); await null; log.push('a2 after'); return 2; }
async function a3() { log.push('a3 start'); throw new Error('x'); }
async function a4() { log.push('a4'); return {then(r) { log.push('then'); r(5); }}; }
async function a5(x) { var y = x; var g = () => y; log.push('a5 ' + g()); await 0; y = 7; return g(); }
async function a6() { try { throw 1 } catch (e) { log.push('a6 caught') } await 0; return 6; }
async function a7() { log.push(typeof this); return arguments.length; }
async function a8() { await 0; return arguments.length; }
async function a9(a, b = a + 1, c) { log.push('a9 ' + a + b + c); await 0; return a + b; }
async function a10() { await new Promise(r => r(10)); var p = Promise.resolve(11); p.then = undefined; return await p; }
var obj = {v: 3, async m() { log.push('m ' + this.v); await 0; return this.v; }};
async function rec(n) { return n ? rec(n - 1) : 'deep'; }
async function inner() { return 'in'; }
async function outer() { var a = await inner(); var b = await inner(); return a + b; }
async function main() {
  log.push('main');
  var p1 = a1(); log.push('after a1');
  var p2 = a2(); log.push('after a2');
  var p3 = a3(); log.push('after a3');
  var p4 = a4(); log.push('after a4');
  var p5 = a5(4);
  var p6 = a6();
  var p7 = a7(1, 2);
  var p8 = a8(1, 2, 3);
  var p9 = a9(1);
  var p10 = a10();
  var pm = obj.m();
  p1.then(v => log.push('p1 ' + v));
  p2.then(v => log.push('p2 ' + v));
  p3.catch(e => log.push('p3 ' + e.message));
  p4.then(v => log.push('p4 ' + v));
  p5.then(v => log.push('p5 ' + v));
  p6.then(v => log.push('p6 ' + v));
  p7.then(v => log.push('p7 ' + v));
  p8.then(v => log.push('p8 ' + v));
  p9.then(v => log.push('p9 ' + v));
  p10.then(v => log.push('p10 ' + v));
  pm.then(v => log.push('pm ' + v));
  rec(100).then(v => log.push('rec ' + v));
  outer().then(v => log.push('outer ' + v));
  Promise.resolve().then(() => log.push('tick1')).then(() => log.push('tick2')).then(() => log.push('tick3'));
  log.push('main end');
  var s = 0;
  for (var i = 0; i < 3; i++) s += await a1();
  log.push('sum ' + s);
}
main();
