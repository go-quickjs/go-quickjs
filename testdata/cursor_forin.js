var R = {};
R.breaks = (function () {
  var o = {a: 1, b: 2, c: 3}, out = [];
  for (var i = 0; i < 3; i++) {
    for (var k in o) { out.push(k); if (k === 'b') break; }
    for (var m in o) { out.push(m); for (var n in o) { out.push(n); break; } }
  }
  return out.join('');
})();
R.finallyContinue = (function () {
  var o = {a: 1, b: 2, c: 3}, out = [];
  for (var k in o) { try { out.push(k); if (k === 'a') break; } finally { if (k === 'a') continue; } }
  for (var k2 in o) out.push(k2);
  return out.join('');
})();
R.returns = (function () {
  var o = {a: 1, b: 2, c: 3}, out = [];
  function f(x) { for (var k in x) { if (k === 'b') return k; } return '-'; }
  for (var k in o) { out.push(k, f(o)); }
  return out.join('');
})();
R.labeled = (function () {
  var o = {a: 1, b: 2}, out = [];
  outer: for (var i = 0; i < 3; i++) {
    for (var k in o) { out.push(i + k); if (k === 'a') continue outer; }
  }
  for (var k3 in o) out.push(k3);
  return out.join(',');
})();
R.chain = (function () {
  var o = {a: 1}, out = [];
  function keys(x) { var r = []; for (var k in x) r.push(k); return r.join(''); }
  out.push(keys(o));
  Object.prototype.z = 1; out.push(keys(o));
  delete Object.prototype.z; out.push(keys(o));
  Object.prototype[0] = 1; out.push(keys(o));
  delete Object.prototype[0]; out.push(keys(o));
  Object.defineProperty(Object.prototype, 'toString', {enumerable: true}); out.push(keys(o));
  Object.defineProperty(Object.prototype, 'toString', {enumerable: false}); out.push(keys(o));
  var p = {q: 1}; Object.setPrototypeOf(o, p); out.push(keys(o));
  Object.setPrototypeOf(o, Object.prototype); out.push(keys(o));
  Array.prototype.w = 1; out.push(keys([5, 6])); delete Array.prototype.w; out.push(keys([5, 6]));
  return out.join(',');
})();
R.throws = (function () {
  var o = {a: 1, b: 2}, out = [];
  for (var i = 0; i < 2; i++) {
    try { for (var k in o) { out.push(k); throw 1; } } catch (e) { out.push('c'); }
  }
  for (var k2 in o) out.push(k2);
  return out.join('');
})();
R.gen = (function () {
  var o = {a: 1, b: 2, c: 3};
  function* g() { for (var k in o) { yield k; } }
  var it = g(), out = [it.next().value];
  for (var k in o) { out.push(k); break; }
  for (var k2 in o) { out.push(k2); if (k2 == 'b') break; }
  out.push(it.next().value, it.next().value, String(it.next().done));
  return out.join('');
})();
R.deleted = (function () {
  var o = {a: 1, b: 2, c: 3}, out = [];
  for (var i = 0; i < 2; i++) { for (var k in o) { out.push(k); if (k === 'a') { delete o.b; break; } } o.b = 2; }
  for (var k2 in o) { out.push(k2); if (k2 === 'a') delete o.c; }
  return out.join('');
})();
R.dict = (function () {
  var d = {z: 0, a: 1, b: 2, c: 3}, out = [];
  delete d.z;
  function keys(x) { var r = ''; for (var k in x) r += k; return r; }
  for (var i = 0; i < 2; i++) out.push(keys(d));
  d.e = 5; out.push(keys(d));
  delete d.a; out.push(keys(d));
  Object.defineProperty(d, 'b', {enumerable: false}); out.push(keys(d));
  Object.defineProperty(d, 'b', {enumerable: true}); out.push(keys(d));
  d.a = 1; out.push(keys(d));
  var seen = '';
  for (var k in d) { seen += k; if (k === 'c') { delete d.e; d.f = 6; } }
  out.push(seen, keys(d));
  d[7] = 1; out.push(keys(d));
  delete d[7]; out.push(keys(d));
  var e = {y: 1, x: 2}; delete e.y; out.push(keys(e), keys(d), keys(e));
  return out.join(',');
})();
JSON.stringify(R)
