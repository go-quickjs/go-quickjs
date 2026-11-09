;; Bulk memory: each round fills 1 MiB with memory.fill, copies it twice
;; with memory.copy, once over itself, and makes 16,384 copies of 64 bytes,
;; the size a program's small copies are.
(module
  (memory 64)

  (func $round (param $r i32) (result i64)
    (local $i i32) (local $acc i64)
    (memory.fill (i32.const 0) (local.get $r) (i32.const 1048576))
    (i32.store (i32.const 4096) (i32.mul (local.get $r) (i32.const 0x01000193)))
    (memory.copy (i32.const 1048576) (i32.const 0) (i32.const 1048576))
    (memory.copy
      (i32.add (i32.const 2097152) (i32.and (local.get $r) (i32.const 255)))
      (i32.const 1048576) (i32.const 1048576))
    ;; Overlapping, the destination after the source.
    (memory.copy (i32.const 1048577) (i32.const 1048576) (i32.const 65536))
    (loop $l
      (memory.copy
        (i32.add (i32.const 3145728) (i32.and (i32.mul (local.get $i) (i32.const 64)) (i32.const 0xFFFF)))
        (i32.and (i32.add (i32.mul (local.get $i) (i32.const 72)) (local.get $r)) (i32.const 0xFFFFF))
        (i32.const 64))
      (local.set $acc (i64.add (local.get $acc) (i64.load (i32.const 3145728))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $i) (i32.const 16384))))
    (i64.add (local.get $acc)
      (i64.add (i64.load (i32.add (i32.const 2101248) (i32.and (local.get $r) (i32.const 255))))
               (i64.load8_u (i32.const 1052673)))))

  (func (export "run") (param $rounds i32) (result i64)
    (local $r i32) (local $acc i64)
    (loop $l
      (local.set $acc (i64.xor (i64.rotl (local.get $acc) (i64.const 7)) (call $round (local.get $r))))
      (local.set $r (i32.add (local.get $r) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $r) (local.get $rounds))))
    (local.get $acc)))
