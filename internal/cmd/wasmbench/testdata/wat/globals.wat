;; Globals: a loop that reads and writes mutable i32, i64 and f64 globals
;; and no locals but its counter, the kernel called 100 times.
(module
  (global $count (mut i32) (i32.const 0))
  (global $sum (mut i64) (i64.const 0))
  (global $x (mut f64) (f64.const 0))

  (func $kernel (param $n i32)
    (local $i i32)
    (loop $l
      (global.set $count (i32.add (global.get $count) (i32.const 1)))
      (global.set $sum
        (i64.add (global.get $sum)
          (i64.extend_i32_u (i32.xor (global.get $count) (local.get $i)))))
      (global.set $x
        (f64.add (f64.mul (global.get $x) (f64.const 0.5)) (f64.convert_i32_u (local.get $i))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $i) (local.get $n)))))

  (func (export "run") (param $n i32) (result i64)
    (local $k i32)
    (global.set $count (i32.const 0))
    (global.set $sum (i64.const 0))
    (global.set $x (f64.const 0))
    (loop $l
      (call $kernel (local.get $n))
      (local.set $k (i32.add (local.get $k) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $k) (i32.const 100))))
    (i64.xor (global.get $sum) (i64.reinterpret_f64 (global.get $x)))))
