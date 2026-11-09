;; Memory traffic a byte and a word at a time: each round fills 1 MiB a byte
;; at a time, xors it a word at a time, and sums it a byte at a time.
(module
  (memory 16)

  (func $fill (param $r i32)
    (local $i i32)
    (loop $l
      (i32.store8 (local.get $i)
        (i32.add (i32.mul (local.get $i) (i32.const 7)) (local.get $r)))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $i) (i32.const 1048576)))))

  (func $xorWords (param $r i32)
    (local $i i32)
    (loop $l
      (i32.store (local.get $i)
        (i32.xor (i32.load (local.get $i)) (i32.add (local.get $i) (local.get $r))))
      (local.set $i (i32.add (local.get $i) (i32.const 4)))
      (br_if $l (i32.lt_u (local.get $i) (i32.const 1048576)))))

  (func $sum (result i32)
    (local $i i32) (local $s i32)
    (loop $l
      (local.set $s (i32.add (local.get $s) (i32.load8_u (local.get $i))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $i) (i32.const 1048576))))
    (local.get $s))

  (func (export "run") (param $rounds i32) (result i64)
    (local $r i32) (local $acc i64)
    (loop $l
      (call $fill (local.get $r))
      (call $xorWords (local.get $r))
      (local.set $acc (i64.add (local.get $acc) (i64.extend_i32_u (call $sum))))
      (local.set $r (i32.add (local.get $r) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $r) (local.get $rounds))))
    (local.get $acc)))
