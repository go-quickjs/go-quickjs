;; f64 arithmetic and memory: the n-body simulation of the Sun and the four
;; giant planets, steps of 0.01 days, a step a call. The bodies are in memory,
;; 56 bytes each: x, y, z, vx, vy, vz and mass. The result is the system's
;; energy at the end, its bits as an i64.
(module
  (memory 1)

  (func $body (param $p i32) (param $x f64) (param $y f64) (param $z f64)
              (param $vx f64) (param $vy f64) (param $vz f64) (param $m f64)
    (f64.store offset=0 (local.get $p) (local.get $x))
    (f64.store offset=8 (local.get $p) (local.get $y))
    (f64.store offset=16 (local.get $p) (local.get $z))
    (f64.store offset=24 (local.get $p) (f64.mul (local.get $vx) (f64.const 365.24)))
    (f64.store offset=32 (local.get $p) (f64.mul (local.get $vy) (f64.const 365.24)))
    (f64.store offset=40 (local.get $p) (f64.mul (local.get $vz) (f64.const 365.24)))
    (f64.store offset=48 (local.get $p) (f64.mul (local.get $m) (call $solarMass))))

  (func $solarMass (result f64)
    (f64.mul (f64.mul (f64.const 4) (f64.const 3.141592653589793)) (f64.const 3.141592653589793)))

  (func $init
    (local $i i32) (local $p i32) (local $px f64) (local $py f64) (local $pz f64) (local $m f64)
    ;; The Sun.
    (call $body (i32.const 0)
      (f64.const 0) (f64.const 0) (f64.const 0) (f64.const 0) (f64.const 0) (f64.const 0)
      (f64.const 1))
    ;; Jupiter.
    (call $body (i32.const 56)
      (f64.const 4.84143144246472090e+00) (f64.const -1.16032004402742839e+00)
      (f64.const -1.03622044471123109e-01) (f64.const 1.66007664274403694e-03)
      (f64.const 7.69901118419740425e-03) (f64.const -6.90460016972063023e-05)
      (f64.const 9.54791938424326609e-04))
    ;; Saturn.
    (call $body (i32.const 112)
      (f64.const 8.34336671824457987e+00) (f64.const 4.12479856412430479e+00)
      (f64.const -4.03523417114321381e-01) (f64.const -2.76742510726862411e-03)
      (f64.const 4.99852801234917238e-03) (f64.const 2.30417297573763929e-05)
      (f64.const 2.85885980666130812e-04))
    ;; Uranus.
    (call $body (i32.const 168)
      (f64.const 1.28943695621391310e+01) (f64.const -1.51111514016986312e+01)
      (f64.const -2.23307578892655734e-01) (f64.const 2.96460137564761618e-03)
      (f64.const 2.37847173959480950e-03) (f64.const -2.96589568540237556e-05)
      (f64.const 4.36624404335156298e-05))
    ;; Neptune.
    (call $body (i32.const 224)
      (f64.const 1.53796971148509165e+01) (f64.const -2.59193146099879641e+01)
      (f64.const 1.79258772950371181e-01) (f64.const 2.68067772490389322e-03)
      (f64.const 1.62824170038242295e-03) (f64.const -9.51592254519715870e-05)
      (f64.const 5.15138902046611451e-05))
    ;; Offset the Sun's momentum so that the system's is zero.
    (loop $l
      (local.set $p (i32.mul (local.get $i) (i32.const 56)))
      (local.set $m (f64.load offset=48 (local.get $p)))
      (local.set $px (f64.add (local.get $px) (f64.mul (f64.load offset=24 (local.get $p)) (local.get $m))))
      (local.set $py (f64.add (local.get $py) (f64.mul (f64.load offset=32 (local.get $p)) (local.get $m))))
      (local.set $pz (f64.add (local.get $pz) (f64.mul (f64.load offset=40 (local.get $p)) (local.get $m))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $i) (i32.const 5))))
    (f64.store offset=24 (i32.const 0) (f64.div (f64.neg (local.get $px)) (call $solarMass)))
    (f64.store offset=32 (i32.const 0) (f64.div (f64.neg (local.get $py)) (call $solarMass)))
    (f64.store offset=40 (i32.const 0) (f64.div (f64.neg (local.get $pz)) (call $solarMass))))

  (func $advance (param $dt f64)
    (local $i i32) (local $j i32) (local $bi i32) (local $bj i32)
    (local $dx f64) (local $dy f64) (local $dz f64) (local $d2 f64) (local $mag f64)
    (local $mi f64) (local $mj f64)
    (loop $li
      (local.set $bi (i32.mul (local.get $i) (i32.const 56)))
      (local.set $mi (f64.load offset=48 (local.get $bi)))
      (local.set $j (i32.add (local.get $i) (i32.const 1)))
      (block $jdone
        (loop $lj
          (br_if $jdone (i32.ge_u (local.get $j) (i32.const 5)))
          (local.set $bj (i32.mul (local.get $j) (i32.const 56)))
          (local.set $mj (f64.load offset=48 (local.get $bj)))
          (local.set $dx (f64.sub (f64.load offset=0 (local.get $bi)) (f64.load offset=0 (local.get $bj))))
          (local.set $dy (f64.sub (f64.load offset=8 (local.get $bi)) (f64.load offset=8 (local.get $bj))))
          (local.set $dz (f64.sub (f64.load offset=16 (local.get $bi)) (f64.load offset=16 (local.get $bj))))
          (local.set $d2
            (f64.add (f64.add (f64.mul (local.get $dx) (local.get $dx))
                              (f64.mul (local.get $dy) (local.get $dy)))
                     (f64.mul (local.get $dz) (local.get $dz))))
          (local.set $mag
            (f64.div (local.get $dt) (f64.mul (local.get $d2) (f64.sqrt (local.get $d2)))))
          (f64.store offset=24 (local.get $bi)
            (f64.sub (f64.load offset=24 (local.get $bi))
                     (f64.mul (f64.mul (local.get $dx) (local.get $mj)) (local.get $mag))))
          (f64.store offset=32 (local.get $bi)
            (f64.sub (f64.load offset=32 (local.get $bi))
                     (f64.mul (f64.mul (local.get $dy) (local.get $mj)) (local.get $mag))))
          (f64.store offset=40 (local.get $bi)
            (f64.sub (f64.load offset=40 (local.get $bi))
                     (f64.mul (f64.mul (local.get $dz) (local.get $mj)) (local.get $mag))))
          (f64.store offset=24 (local.get $bj)
            (f64.add (f64.load offset=24 (local.get $bj))
                     (f64.mul (f64.mul (local.get $dx) (local.get $mi)) (local.get $mag))))
          (f64.store offset=32 (local.get $bj)
            (f64.add (f64.load offset=32 (local.get $bj))
                     (f64.mul (f64.mul (local.get $dy) (local.get $mi)) (local.get $mag))))
          (f64.store offset=40 (local.get $bj)
            (f64.add (f64.load offset=40 (local.get $bj))
                     (f64.mul (f64.mul (local.get $dz) (local.get $mi)) (local.get $mag))))
          (local.set $j (i32.add (local.get $j) (i32.const 1)))
          (br $lj)))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $li (i32.lt_u (local.get $i) (i32.const 5))))
    (local.set $i (i32.const 0))
    (loop $lp
      (local.set $bi (i32.mul (local.get $i) (i32.const 56)))
      (f64.store offset=0 (local.get $bi)
        (f64.add (f64.load offset=0 (local.get $bi)) (f64.mul (local.get $dt) (f64.load offset=24 (local.get $bi)))))
      (f64.store offset=8 (local.get $bi)
        (f64.add (f64.load offset=8 (local.get $bi)) (f64.mul (local.get $dt) (f64.load offset=32 (local.get $bi)))))
      (f64.store offset=16 (local.get $bi)
        (f64.add (f64.load offset=16 (local.get $bi)) (f64.mul (local.get $dt) (f64.load offset=40 (local.get $bi)))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $lp (i32.lt_u (local.get $i) (i32.const 5)))))

  (func $energy (result f64)
    (local $e f64) (local $i i32) (local $j i32) (local $bi i32) (local $bj i32)
    (local $dx f64) (local $dy f64) (local $dz f64) (local $vx f64) (local $vy f64) (local $vz f64)
    (loop $li
      (local.set $bi (i32.mul (local.get $i) (i32.const 56)))
      (local.set $vx (f64.load offset=24 (local.get $bi)))
      (local.set $vy (f64.load offset=32 (local.get $bi)))
      (local.set $vz (f64.load offset=40 (local.get $bi)))
      (local.set $e
        (f64.add (local.get $e)
          (f64.mul (f64.mul (f64.const 0.5) (f64.load offset=48 (local.get $bi)))
            (f64.add (f64.add (f64.mul (local.get $vx) (local.get $vx))
                              (f64.mul (local.get $vy) (local.get $vy)))
                     (f64.mul (local.get $vz) (local.get $vz))))))
      (local.set $j (i32.add (local.get $i) (i32.const 1)))
      (block $jdone
        (loop $lj
          (br_if $jdone (i32.ge_u (local.get $j) (i32.const 5)))
          (local.set $bj (i32.mul (local.get $j) (i32.const 56)))
          (local.set $dx (f64.sub (f64.load offset=0 (local.get $bi)) (f64.load offset=0 (local.get $bj))))
          (local.set $dy (f64.sub (f64.load offset=8 (local.get $bi)) (f64.load offset=8 (local.get $bj))))
          (local.set $dz (f64.sub (f64.load offset=16 (local.get $bi)) (f64.load offset=16 (local.get $bj))))
          (local.set $e
            (f64.sub (local.get $e)
              (f64.div
                (f64.mul (f64.load offset=48 (local.get $bi)) (f64.load offset=48 (local.get $bj)))
                (f64.sqrt
                  (f64.add (f64.add (f64.mul (local.get $dx) (local.get $dx))
                                    (f64.mul (local.get $dy) (local.get $dy)))
                           (f64.mul (local.get $dz) (local.get $dz)))))))
          (local.set $j (i32.add (local.get $j) (i32.const 1)))
          (br $lj)))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $li (i32.lt_u (local.get $i) (i32.const 5))))
    (local.get $e))

  (func (export "run") (param $steps i32) (result i64)
    (local $k i32)
    (call $init)
    (block $done
      (loop $l
        (br_if $done (i32.ge_u (local.get $k) (local.get $steps)))
        (call $advance (f64.const 0.01))
        (local.set $k (i32.add (local.get $k) (i32.const 1)))
        (br $l)))
    (i64.reinterpret_f64 (call $energy))))
