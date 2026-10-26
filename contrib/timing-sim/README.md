# Timing simulator

Compares block-timestamp rule sets under faults and attacks. Design:
`doc/superpowers/specs/2026-10-26-timing-harness-design.md`.

The simulator never re-implements the difficulty or header rules for R0 and R1. It
asks the real C++ code, through `timing_ref`, a small binary that runs
`GetNextWorkRequired()` and `ContextualCheckBlockHeader()` on synthetic chains.

## Rule sets

| Name | Rules | Where it runs |
|------|-------|---------------|
| R0 | Today's: 2-hour future drift, spacing floor (default 4s), LWMA | C++ (`-timingprofile=whippet`) |
| R1 | R0 with a 72s drift (12 block times) | C++ (`-timingprofile=scaled-drift`) |
| R2 | Dogecoin-style: later than the median of the last 11, no floor, 2-hour drift | **sim-only** |
| R3 | R1 with the floor off inside a height window (`rule_params.R3.window`) | **sim-only** |
| R4 | R1 plus a cap on each step from the parent (`rule_params.R4.cap_multiple` x 6s) | **sim-only** |
| R2c | R2 with LWMA solvetimes floored at 1s (`lwma.py`, `min_solvetime`) | **sim-only** |
| R2m | R2 with LWMA over monotonic timestamps (each raised to the previous + 1, as LWMA-1) | **sim-only** |
| R5 | No wall clock: reject a block at or before the median of the last 11, or more than `cap_multiple` x 6s after the median of the last `window` blocks. No floor. Honest miners stamp min(clock, limit) | **sim-only** |
| R5c, R5m | R5 with the R2c or R2m LWMA variant | **sim-only** |
| R1a, R2a, R5a | R1, R2 or R5 acceptance rules with ASERT-style difficulty (`asert.py`, `rule_params.<name>.tau_s`, half-life in seconds) | **sim-only** |
| R6, R6d | R2's acceptance (median only, no floor) with the 72s drift cap and ASERT difficulty; R6d adds `tau_wait_s` (default 60): a candidate block more than 6 block times after its parent gets an easier target, doubling every tau_wait seconds, so waiting lowers the difficulty | **sim-only** |

ASERT sets compute `nBits` as `anchor_target * 2**(lateness/tau)`, where lateness is the parent's time minus the schedule from the base block; they use no LWMA at all, so R1a re-implements R1's acceptance rules in Python.

Sim-only rows implement the changed timestamp rule in `rules.py` (marked `SIM-ONLY RULE`). R2, R3 and R4 use the real
C++ LWMA for `nBits`; the `c`/`m` variants use `lwma.py`, a line-for-line Python port of the C++ LWMA
(including its uint32 truncation of a negative weighted sum), checked against C++ in `tests/test_lwma.py`. Reports label them "sim-only, not
cross-checked against C++".

## Build and run

Native builds fail on this host, so build in the image (see `NEXT-STEPS.md`):

    docker run --rm -u $(id -u):$(id -g) -e CCACHE_DIR=/ccache \
      -v "$PWD:/src" -v /tmp/whippet-ccache:/ccache --entrypoint bash whippet-ubuntu24 \
      -c 'cd /src && ./autogen.sh && ./configure --without-gui --disable-bench \
          && make -j$(nproc) -C src whippetd whippet-cli test/test_whippet test/timing_ref'

The Python side runs on the host and starts `timing_ref` with `docker run -i`. Set
`WHIPPET_TIMING_REF=/path/to/timing_ref` to use a native binary instead.

    cd contrib/timing-sim
    python3 -m pytest tests/                                   # all tests (about 1 minute)
    python3 sim.py scenarios/far_future.json                   # one scenario, one line per rule set
    python3 report.py scenarios/*.json --seed 1 --out DIR      # tables and CSVs (--plots for PNGs)
    python3 multi_seed.py scenarios/burst_pump.json --seeds 5 --jobs 5   # median and worst case over seeds
    python3 confirm_regtest.py --scenario far_future --rules R0   # replay on real regtest nodes

## Scenarios

JSON files in `scenarios/`. A miner is `{name, hashrate, clock_offset, strategy, params,
active_from, active_until}`. Hashrate 1.0 finds a block every 6s at the base target.
Strategies: `honest`, `far_future`, `walk_ahead`, `min_diff_farmer`, `pump` (stamp as early as the rules allow). `params.max_per_second` lets a miner find several blocks in one second (a burst). Scenarios: `far_future`, `walk_ahead`, `min_diff_farm`, `skewed_clock`, `hashrate_jump`, `burst_and_leave` / `burst_pump` (a 100x miner for 300s, then gone; `burst_end_s` makes the `post_burst_gap` metric), `outage` (all honest miners off for a while, then back: checks liveness), `steady`. `legacy_miner: true`
makes honest miners stamp the way the pre-fix `UpdateTime()` did (ignoring the floor),
which reproduces the tester's halt. `sweep` runs one scenario per attacker hashrate.
`params.scripted_every_s` replaces random arrivals with a fixed schedule (used to match
the real-node replay).

## Real-node confirmation

`confirm_regtest.py` starts two regtest nodes with `-timingprofile`, drives them with
`setmocktime`, and prints the result next to the simulator's for the same schedule.
It covers R0 and R1 only, and two scenarios (`far_future`, `walk_ahead`). A mismatch
is printed as a finding and does not fail the run. Real nodes only have the fixed
miner, so the legacy-miner halt cannot be reproduced there. Regtest proof of work is
trivial, so difficulty feedback is not exercised; the comparison is on timestamp lead
and stall.

## Known limits

- No network latency or orphaning: a block reaches every miner instantly.
- Below height 145000 the code retargets only every 10 blocks. Synthetic chains start at
  145000 (mainnet's every-block regime), but real regtest nodes run at low heights.
- Attackers stamp as far ahead as the rules allow but do not otherwise adapt.
- R5 needs `cap_multiple` comfortably above `window`/2 (the median of the last N blocks lags the tip by N/2 blocks), or honest blocks cannot satisfy both limits and the chain halts.
- The real-node replay (`confirm_regtest.py`) is limited to about 1200s: regtest keeps making blocks harder under a fixed schedule.
- Peer-offset clock skew (the 70-minute clamp) is not modelled; a skewed miner is a fixed
  `clock_offset`.
