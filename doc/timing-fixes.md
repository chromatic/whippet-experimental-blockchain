# Block timestamp fixes: approach and numbers to validate

Branch `review/timing-fixes` (from `feature/testnet`, plus the soft fork commit). Nothing here is released.

## The problem

A block's timestamp is chosen by its miner. Three rules constrain it: it must exceed the median of the last 11 blocks, it must be at least `nMinBlockSpacing` (4s since height 100000) after its parent, and it must be at most `nMaxFutureDrift` (2 hours) ahead of the node's adjusted time. The spacing floor is relative to the parent, so one future-stamped block forces every descendant to be stamped at or after it. A miner that stamps each block a little ahead of the last drags the chain clock away from real time, and LWMA, which reads those stamps, loses track of the true hashrate.

## What is on the branch

1. **Miner fix** (`ab4b3c8eb`). `UpdateTime()` ignored the spacing floor, so after a future-stamped block every template was too early until the clock caught up. It now stamps `max(MTP+1, parent.time + nMinBlockSpacing, adjusted)`. This was the cause of the tester's 5-minute halt.
2. **Drift is a parameter** (`68c3a409d`). The literal 2 hours became `Consensus::Params::nMaxFutureDrift`, plus regtest timing profiles and a `timing_ref` test binary.
3. **Soft fork: 72s drift cap** (`89233fb96`). From `MAINNET_DRIFT_CAP_HEIGHT` a block may be at most 72s (12 block times) ahead of adjusted time. The rule is strictly tighter, so old nodes accept everything new nodes accept; only miners stamping more than 72s ahead are affected. `-maxtimeadjustment` drops from 70 minutes to 36s so peers cannot move a node's clock across the 72s window (node policy, not consensus).
4. **Testnet** (`924fa431d`, plus the testnet edits in `89233fb96`). Own genesis, Dogecoin leftovers removed, 4s floor and 72s cap from genesis (a floor equal to the 6s target blinds LWMA: every solvetime is clamped to the target and difficulty never moves).
5. **Simulator** (`contrib/timing-sim/`, see its README), including sim-only candidates for a later hard fork. They are not implemented in C++ and are not part of the soft fork.

`MAINNET_DRIFT_CAP_HEIGHT = 360000` is a **placeholder**. It has to be late enough that most hashrate has upgraded.

## Why 72s

The cap was swept with the same rules in Python (`R1x`) and, at 72s, in the real C++ (`R1`); the two match block for block. Below 72s, honest miners stall more under attack (a block stamped near the cap makes the next honest block exceed it until clocks catch up). Above it, a 1000x miner that bursts and leaves is no longer bounded. 72s is the best of the values tried, from 3 seeds.

## Numbers to validate

All simulator runs are seeded, so re-running regenerates them. Median over seeds, worst case in brackets, seconds unless stated.

Build and run (see `contrib/timing-sim/README.md`):

    docker run --rm -u $(id -u):$(id -g) -e CCACHE_DIR=/ccache \
      -v "$PWD:/src" -v /tmp/whippet-ccache:/ccache --entrypoint bash whippet-ubuntu24 \
      -c 'cd /src && ./autogen.sh && ./configure --without-gui --disable-bench \
          && make -j$(nproc) -C src whippetd whippet-cli test/test_whippet test/timing_ref'
    docker run --rm -u $(id -u):$(id -g) -v "$PWD:/src" --entrypoint bash whippet-ubuntu24 \
      -c 'cd /src && ./src/test/test_whippet --run_test=timing_tests,chainparams_tests,pow_tests'
    cd contrib/timing-sim
    python3 sweep.py sweeps/soft_fork.json --jobs 8 --out OUT
    python3 sim.py scenarios/walk_ahead.json 1
    python3 sim.py scenarios/far_future.json 1

Unit tests: 22 cases in `timing_tests`, `chainparams_tests`, `pow_tests`, no errors (run on this branch, 2026-10-27). They cover the cap on each side of the activation height.

### Cap value (`sweeps/soft_fork.json`, 3 seeds)

| Cap | +300s block: lead | walk-ahead 0.3: attacker share (hash share 0.231) | honest stall, 1h at 0.3 | wait after 1000x burst |
|-----|----|----|----|----|
| none (R0) | 332 (335) | 0.38 (0.38) | 409 (463) | 2700 (2700) |
| 36s | 24 (29) | 0.23 (0.25) | 712 (744) | 463 (1302) |
| 48s | 24 (29) | 0.24 (0.25) | 711 (717) | 463 (1302) |
| **72s** | 24 (29) | 0.26 (0.28) | **250 (254)** | 463 (1302) |
| 120s | 24 (29) | 0.24 (0.26) | 337 (350) | 1302 (2700) |
| 180s | 24 (29) | 0.24 (0.26) | 326 (376) | 1302 (2700) |

The "C++ R1" row of the sweep reproduces the 72s row exactly.

### Walk-ahead attacker, 0.2 hashrate, 2 hours, seed 1 (`sim.py scenarios/walk_ahead.json 1`)

| Rules | blocks/h | max lead | attacker share (hash share 0.167) | honest stall |
|-------|----------|----------|------|------|
| R0 (today) | 764 | 7200s | 0.325 | 221s |
| R1 (72s cap) | 551 | 72s | 0.197 | 418s |

The cap brings a walk-ahead attacker from about twice its hashrate share down to roughly its share, and stops the chain clock running two hours ahead. The price is honest stall, quoted above.

### Far-future block, +300s (seed 1)

R0: max lead 300s, longest gap 306s. R1: max lead 0, longest gap 22s (the 300s block is refused).

## What this does not fix

LWMA still reacts badly to a very large miner that leaves. Burst of 100x for 300s, then gone, median wait for the next block: 1079s under today's rules and under the cap (worst 2700s today, 1742s under the cap; 5 seeds, 145000 regime, `b100x gap` in `sweeps/round3.json`). Fixing that needs a difficulty change, which is a hard fork. Two candidates are simulated (5 seeds, `sweeps/round3.json`): LWMA plus easing while waiting (`R1e`, burst wait 195-233s) and ASERT with easing at tau 30-60 (`R6d`, 116s). Neither has C++ yet.

Height 145000 is a separate, existing change: LWMA retargets every block from there instead of every 10, and the simulator shows burst-and-leave getting worse across it (R0 median 463s below, 1079s above).

## Limits of the evidence

- No network latency or orphaning; attackers stamp as far ahead as allowed but do not otherwise adapt.
- Five seeds at most (three for the cap sweep), and one for the walk-ahead table.
- Real-node confirmation covers R0 and R1 only, far-future and walk-ahead scenarios, 1200s (`confirm_regtest.py`).
- Peer-offset clock skew and merge-mining job staleness are not modelled.
- A rehearsal with one old and one new binary across the activation height has not been done.
