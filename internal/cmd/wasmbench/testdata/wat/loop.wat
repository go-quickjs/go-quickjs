;; A tight loop of i32 arithmetic on locals, the kernel called 100 times.
(module
  (func $kernel (param $n i32) (param $seed i32) (result i32)
    (local $i i32) (local $acc i32)
    (local.set $acc (local.get $seed))
    (block $done
      (loop $l
        (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
        (local.set $acc
          (i32.add
            (i32.mul (local.get $acc) (i32.const 33))
            (i32.xor (local.get $i) (i32.shr_u (local.get $i) (i32.const 3)))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $l)))
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
