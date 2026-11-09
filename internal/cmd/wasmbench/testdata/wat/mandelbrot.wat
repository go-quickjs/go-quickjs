;; f64 arithmetic: the Mandelbrot set over an n-by-n grid of [-1.5, 0.5] x
;; [-1, 1], at most 50 iterations a point, a row a call. The result is the
;; total of the iterations.
(module
  (func $row (param $y i32) (param $size i32) (result i32)
    (local $x i32) (local $cr f64) (local $ci f64) (local $zr f64) (local $zi f64)
    (local $tr f64) (local $it i32) (local $sum i32)
    (local.set $ci
      (f64.sub
        (f64.div (f64.mul (f64.const 2) (f64.convert_i32_u (local.get $y)))
                 (f64.convert_i32_u (local.get $size)))
        (f64.const 1)))
    (block $xdone
      (loop $xl
        (br_if $xdone (i32.ge_u (local.get $x) (local.get $size)))
        (local.set $cr
          (f64.sub
            (f64.div (f64.mul (f64.const 2) (f64.convert_i32_u (local.get $x)))
                     (f64.convert_i32_u (local.get $size)))
            (f64.const 1.5)))
        (local.set $zr (f64.const 0))
        (local.set $zi (f64.const 0))
        (local.set $it (i32.const 0))
        (block $esc
          (loop $il
            (br_if $esc (i32.ge_u (local.get $it) (i32.const 50)))
            (local.set $tr
              (f64.add
                (f64.sub (f64.mul (local.get $zr) (local.get $zr))
                         (f64.mul (local.get $zi) (local.get $zi)))
                (local.get $cr)))
            (local.set $zi
              (f64.add
                (f64.mul (f64.mul (f64.const 2) (local.get $zr)) (local.get $zi))
                (local.get $ci)))
            (local.set $zr (local.get $tr))
            (br_if $esc
              (f64.gt
                (f64.add (f64.mul (local.get $zr) (local.get $zr))
                         (f64.mul (local.get $zi) (local.get $zi)))
                (f64.const 4)))
            (local.set $it (i32.add (local.get $it) (i32.const 1)))
            (br $il)))
        (local.set $sum (i32.add (local.get $sum) (local.get $it)))
        (local.set $x (i32.add (local.get $x) (i32.const 1)))
        (br $xl)))
    (local.get $sum))

  (func (export "run") (param $size i32) (result i64)
    (local $y i32) (local $acc i64)
    (block $done
      (loop $l
        (br_if $done (i32.ge_u (local.get $y) (local.get $size)))
        (local.set $acc
          (i64.add (local.get $acc)
            (i64.extend_i32_u (call $row (local.get $y) (local.get $size)))))
        (local.set $y (i32.add (local.get $y) (i32.const 1)))
        (br $l)))
    (local.get $acc)))
