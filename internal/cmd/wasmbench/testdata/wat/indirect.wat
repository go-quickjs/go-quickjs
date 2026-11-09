;; call_indirect dispatch: each iteration calls one of eight small functions
;; through a table, picked by the data, the kernel called 100 times.
(module
  (type $op (func (param i32) (result i32)))
  (table 8 funcref)
  (elem (i32.const 0) $f0 $f1 $f2 $f3 $f4 $f5 $f6 $f7)

  (func $f0 (type $op) (i32.add (local.get 0) (i32.const 1)))
  (func $f1 (type $op) (i32.xor (local.get 0) (i32.const 0x55)))
  (func $f2 (type $op) (i32.mul (local.get 0) (i32.const 3)))
  (func $f3 (type $op) (i32.sub (local.get 0) (i32.const 7)))
  (func $f4 (type $op) (i32.rotl (local.get 0) (i32.const 5)))
  (func $f5 (type $op) (i32.add (i32.shr_u (local.get 0) (i32.const 1)) (i32.const 0x1234)))
  (func $f6 (type $op) (i32.or (local.get 0) (i32.const 0x100)))
  (func $f7 (type $op) (i32.add (i32.and (local.get 0) (i32.const 0xffff)) (i32.const 0x10000)))

  (func $kernel (param $n i32) (param $seed i32) (result i32)
    (local $i i32) (local $acc i32)
    (local.set $acc (local.get $seed))
    (loop $l
      (local.set $acc
        (call_indirect (type $op)
          (local.get $acc)
          (i32.and
            (i32.xor (local.get $i) (i32.shr_u (local.get $acc) (i32.const 3)))
            (i32.const 7))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $i) (local.get $n))))
    (local.get $acc))

  (func (export "run") (param $n i32) (result i64)
    (local $k i32) (local $acc i64)
    (loop $l
      (local.set $acc
        (i64.add (local.get $acc)
          (i64.extend_i32_u (call $kernel (local.get $n) (local.get $k)))))
      (local.set $k (i32.add (local.get $k) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $k) (i32.const 100))))
    (local.get $acc)))
