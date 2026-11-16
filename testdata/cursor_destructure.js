var R = {};
R.nested = (function () {
  var out = [];
  for (var i = 0; i < 3; i++) {
    var [[a, b], [c, d]] = [[1, 2], [3, 4]];
    var [e = ([x, y] = [7, 8], x), f] = [];
    var x, y;
    out.push(a, b, c, d, e, f, x, y);
  }
  return out.join(',');
})();
R.doneThenGrow = (function () {
  var arr = [1];
  var [a, b = (arr.push(7, 8), 0), c] = arr;
  var [d, ...r] = arr;
  return [a, b, c, d, r.join('')].join(',');
})();
R.throwPath = (function () {
  var out = [];
  for (var i = 0; i < 2; i++) {
    try { var [a, {b}] = [1, null]; } catch (e) { out.push(e.constructor.name); }
    var [c, d] = [i, i + 1]; out.push(c, d);
  }
  return out.join(',');
})();
R.forOfBreak = (function () {
  var out = [], arr = [1, 2, 3];
  for (var i = 0; i < 2; i++) {
    for (var v of arr) { out.push(v); if (v === 2) break; }
    for (var w of arr) { for (var z of arr) { out.push('' + w + z); break; } }
    var [p, q] = arr; out.push(p + q);
  }
  return out.join(',');
})();
R.gen = (function () {
  var arr = [1, 2, 3];
  function* g() { var [a = yield 'y', b] = [undefined, 5]; return a + ':' + b; }
  var it = g(), out = [it.next().value];
  var [m, n] = arr; out.push(m, n);
  for (var v of arr) { if (v == 2) break; }
  var [s, t, u] = [9, 8, 7]; out.push(s, t, u);
  out.push(it.next(4).value);
  return out.join(',');
})();
R.holes = (function () {
  Array.prototype[1] = 'P';
  var [a, b, c] = [1, , 3];
  delete Array.prototype[1];
  var [d, e] = [1, , 3];
  return [a, b, c, d, String(e)].join(',');
})();
R.returnMethod = (function () {
  var AIP = Object.getPrototypeOf([][Symbol.iterator]()), log = [];
  AIP.return = function () { log.push('ret' + (this instanceof Object)); return {}; };
  var [a] = [1, 2];
  var [b, c, d] = [1, 2];
  for (var v of [1, 2]) break;
  delete AIP.return;
  var [e] = [1, 2];
  return log.join(',') + '|' + [a, b, c, String(d), e].join(',');
})();
R.setterTarget = (function () {
  var arr = [1, 2, 3], log = [], o = {};
  Object.defineProperty(o, 'x', {set(v) { log.push('x' + v); var [p, q] = [v * 10, v * 20]; log.push(p + q); }});
  [o.x, o.x, o.x] = arr;
  return log.join(',');
})();
R.shrink = (function () {
  var arr = [1, 2, 3, 4];
  var [a, b = 0, c] = (arr.length = 1, arr);
  var arr2 = [1, 2, 3];
  var [d, e = (arr2.length = 1, 9), f, ...g] = [1, undefined, 3, 4];
  return [a, String(b), String(c), d, e, f, g.join('')].join(',');
})();
JSON.stringify(R)
