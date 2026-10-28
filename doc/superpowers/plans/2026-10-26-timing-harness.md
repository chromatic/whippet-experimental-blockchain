# Timing Harness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Python simulator, backed by the real C++ difficulty and header checks, that compares block-timestamp rule sets under faults and attacks, plus a regtest confirmation for the rule sets that exist in C++.

**Architecture:** Add `nMaxFutureDrift` to consensus params. Add a regtest-only `-timingprofile` argument that makes regtest run Whippet's timing rules. Add a small `timing_ref` binary that answers "required nBits" and "is this header acceptable" over stdin/stdout on synthetic chains. A host-side Python simulator drives it. A regtest script replays three scenarios on real nodes.

**Tech Stack:** C++11 (Boost.Test), autotools, Python 3 (stdlib only plus optional matplotlib), Docker image `whippet-ubuntu24`.

**Spec:** `doc/superpowers/specs/2026-10-26-timing-harness-design.md`. Measurements and background: `NEXT-STEPS.md` (untracked, repo root).

## Global Constraints

- **Do not commit.** The user said not to commit. Leave every change in the working tree.
- Default regtest behaviour must not change: 1s spacing, `nMinBlockSpacing = 0`, `fPowNoRetargeting = true`, LWMA off. `qa/rpc-tests` depends on it.
- `-timingprofile` is accepted on regtest only; any other network, or an unknown value, aborts startup with an error.
- Default `nMaxFutureDrift` is 7200 on mainnet, testnet and regtest. Mainnet and public testnet consensus behaviour must not change.
- Profile values: `nPowTargetSpacing` = 6, floor (`nMinBlockSpacing`) = `-timingfloor` (default 4), LWMA on, `fPowNoRetargeting` = false, `DifficultyAdjustmentInterval()` = 10 (`nPowTargetTimespan` = 60, as testnet's LWMA branch; the LWMA window N equals this interval), `fPowAllowMinDifficultyBlocks` = false. `scaled-drift` also sets `nMaxFutureDrift` = 72 (12 block times).
- A profile applies to all four regtest parameter sets (the root and the three height branches in `CRegTestParams`).
- R2, R3 and R4 are simulator-only. They get no C++ and no profile.
- Native builds fail on this host. Build and run C++ in the container:
  ```
  docker run --rm -u $(id -u):$(id -g) -e CCACHE_DIR=/ccache \
    -v "$PWD:/src" -v /tmp/whippet-ccache:/ccache --entrypoint bash whippet-ubuntu24 \
    -c 'cd /src && ./autogen.sh && ./configure --without-gui --disable-bench \
        && make -j$(nproc) -C src whippetd whippet-cli test/test_whippet test/timing_ref'
  ```
  Later rebuilds need only the `make` part. Run `./src/test/test_whippet` inside the same image.
- Python runs on the host. It reaches `timing_ref` with `docker exec -i <container> /src/src/test/timing_ref`.
- Scratch files go in the session scratchpad, not `/tmp`.

## Review Focus

- A rejected attacker block must not extend the chain or move any miner's tip (Task 5).
- A header exactly at the drift limit is accepted and one second over is rejected, for both 7200 and 72 (Tasks 1, 2).
- A young chain (fewer than 10 blocks) must return the previous block's bits, not crash (Task 3).
- The same seed must give byte-identical CSV output (Task 4).
- A miner with a negative clock offset (clock behind) must not produce time-too-old headers or loop forever (Task 5).

---

## File structure

- Modify `src/consensus/params.h`, `src/chainparams.cpp`, `src/validation.cpp`: drift parameter and profile.
- Modify `src/chainparamsbase.cpp` (help text only) and `src/Makefile.test.include`: help, new test file and binary.
- Create `src/test/timing_tests.cpp`: unit tests for Tasks 1 to 3.
- Create `src/test/timing_ref.cpp`: the reference calculator.
- Create `contrib/timing-sim/`: `refcalc.py` (client), `chain.py`, `rules.py`, `miners.py`, `sim.py`, `scenarios/*.json`, `report.py`, `confirm_regtest.py`, `tests/`.

---

### Task 1: `nMaxFutureDrift` consensus parameter

**Files:**
- Modify: `src/consensus/params.h` (next to `nMinBlockSpacing`), `src/chainparams.cpp` (mainnet, testnet, regtest constructors), `src/validation.cpp:3305`
- Create: `src/test/timing_tests.cpp`; add to `BITCOIN_TESTS` in `src/Makefile.test.include`

**Interfaces:**
- Produces: `int64_t Consensus::Params::nMaxFutureDrift` (seconds), 7200 everywhere by default. `ContextualCheckBlockHeader` signature is unchanged and already takes `nAdjustedTime`.

- [ ] **Step 1: Write the failing test** in `timing_tests.cpp`, suite `timing_tests` (fixture `BasicTestingSetup`): `future_drift_default_boundary`. `SelectParams(CBaseChainParams::REGTEST)`. Build a parent `CBlockIndex` at height 100 with `nTime = T`, then a `CBlockHeader` with `nVersion = 0x00620004`, `nTime = adjusted + 7200`, and `nBits` set by `GetNextWorkRequired`. Assert `ContextualCheckBlockHeader(hdr, state, &parent, adjusted)` is true. With `nTime = adjusted + 7201` assert false and `state.GetRejectReason() == "time-too-new"`. Keep the parent time at least 1s after the median past so no other rule fires.
- [ ] **Step 2:** Add the file to the build, rebuild with the Docker command. Expected: compile error, `nMaxFutureDrift` undefined, or test FAIL (7201 accepted if the field is stubbed).
- [ ] **Step 3: Add `int64_t nMaxFutureDrift;`** to `Consensus::Params` with a one-line doc comment. Set `= 2 * 60 * 60` in the three base constructors. Replace the literal at `validation.cpp:3305` with `consensusParams.nMaxFutureDrift`.
- [ ] **Step 4:** Rebuild and run `./src/test/test_whippet --run_test=timing_tests`. Expected: PASS. Then run the full `./src/test/test_whippet`. Expected: all cases pass (328 before this task plus the new one).

### Task 2: Regtest `-timingprofile`

**Files:**
- Modify: `src/chainparams.cpp` (`CRegTestParams`), `src/chainparamsbase.cpp` (hidden help, debug-only like `-regtest` help)
- Test: `src/test/timing_tests.cpp`

**Interfaces:**
- Consumes: `Consensus::Params::nMaxFutureDrift` from Task 1.
- Produces: arguments `-timingprofile=whippet|scaled-drift` and `-timingfloor=<seconds>` read via `GetArg` in `SelectParams` (regtest only) and applied by a new `CRegTestParams::ApplyTimingProfile(profile, floor)` that follows the existing `UpdateRegtest*` pattern (the static `regTestParams` is constructed before arguments are parsed, so the constructor cannot read them). Applying an empty profile restores the defaults. The profile sets the values listed in Global Constraints on each set.

- [ ] **Step 1: Write failing tests**: `profile_whippet_values` (`ForceSetArg("-timingprofile","whippet")`, `SelectParams(REGTEST)`, then for heights 0, 15, 25, 90000 check spacing 6, `nMinBlockSpacing` 4, `fLWMADifficultyCalculation`, `!fPowNoRetargeting`, `DifficultyAdjustmentInterval() == 10`, `nMaxFutureDrift == 7200`); `profile_scaled_drift_boundary` (profile `scaled-drift`: header at adjusted+72 accepted, adjusted+73 rejected with `time-too-new`); `profile_floor_override` (`-timingfloor=0` gives `nMinBlockSpacing == 0`); `profile_unset_is_unchanged` (spacing 1, floor 0, retargeting off); `profile_unknown_throws` (`-timingprofile=bogus` makes `SelectParams(REGTEST)` throw `std::runtime_error`); `profile_rejected_off_regtest` (`SelectParams(TESTNET)` with the arg set throws). Reset the args in each case, since args are global.
- [ ] **Step 2:** Rebuild, run `--run_test=timing_tests`. Expected: FAIL on the new cases.
- [ ] **Step 3: Implement** `static void ApplyTimingProfile(Consensus::Params&)` in `chainparams.cpp` and call it for each of the four sets at the end of the `CRegTestParams` constructor. Throw `std::runtime_error` for an unknown value. For the non-regtest rejection, check in `CTestNetParams` and `CMainParams` constructors, or in `SelectParams`, that `-timingprofile` is unset, and throw otherwise (one place is enough; choose the least invasive).
- [ ] **Step 4:** Rebuild; run `timing_tests`, then the full suite. Expected: all pass. Also run `qa/rpc-tests` smoke: `qa/pull-tester/rpc-tests.py` is too long; run just `qa/rpc-tests/getblocktemplate_longpoll.py` inside the container to check default regtest is unaffected.

### Task 3: `timing_ref` reference calculator

**Files:**
- Create: `src/test/timing_ref.cpp`
- Modify: `src/Makefile.test.include` (new `noinst_PROGRAMS` entry `test/timing_ref`, same `LDADD`, `CPPFLAGS` and `LDFLAGS` as `test_whippet`, sources `test/timing_ref.cpp`)
- Test: `src/test/timing_tests.cpp` (covers the shared core) and `contrib/timing-sim/tests/test_refcalc.py` (protocol)

**Interfaces:**
- Consumes: Task 2's profile arguments, passed as command-line arguments to `timing_ref` (`-timingprofile=...`, `-timingfloor=...`); they select regtest.
- Produces a line protocol on stdin/stdout. Each request is one line, each reply one line:
  - `RESET [height]` replies `OK`. Clears the chain and makes a synthetic base block at `height` (default 145000, so mainnet's every-block retarget and 4s floor regime applies; time 1767254400, bits 0x207fffff) the tip, index `0`.
  - `APPEND <time> <bits_hex>` replies `OK <index>`. Adds a block on the current tip with that time and bits, sets it as the tip, index counts from 0 in insertion order.
  - `TIP <index>` replies `OK`. Sets the tip for queries and appends; supports forks.
  - `NEXTBITS <time>` replies `BITS <hex>`. Required `nBits` for a block at `<time>` on the tip (`GetNextWorkRequired`).
  - `CHECK <time> <bits_hex> <adjusted_time>` replies `OK` or `REJECT <reason>`. Runs `ContextualCheckBlockHeader` with version `0x00620004`.
  - Unknown command replies `ERR <message>` and keeps running. EOF exits 0.

- [ ] **Step 1: Write failing tests**: a C++ case `refcalc_young_chain_returns_prev_bits` (a 10-block chain under the whippet profile: `GetNextWorkRequired` equals the tip's bits, no crash); a Python test file `test_refcalc.py` using a `RefCalc` class (Task 4 Step 3 defines the client; write the test against its interface now): `reset`, ten `append`s, `next_bits`, `check` accepted, `check` with time one second under the floor rejected with `bad-time-too-early`, `tip(i)` then `append` forks, bogus command returns an error and the process stays alive.
- [ ] **Step 2:** Run. Expected: FAIL (binary missing).
- [ ] **Step 3: Implement `timing_ref.cpp`.** Own the `CBlockIndex` objects in a `std::deque` (stable addresses). Compute `nChainWork` is not needed. Set `pprev`, `nHeight`, `nTime`, `nBits`, and call `BuildSkip()` if `GetMedianTimePast` needs it (it does not; it walks `pprev`).
- [ ] **Step 4:** Rebuild, run `timing_tests`, then `pytest contrib/timing-sim/tests/test_refcalc.py` (needs a running container named `wtn-ref`; the test fixture starts and stops it). Expected: PASS.

### Task 4: Simulator core, R0 and R1, honest steady state

**Files:**
- Create: `contrib/timing-sim/refcalc.py`, `chain.py`, `rules.py`, `miners.py`, `sim.py`, `scenarios/steady.json`, `tests/test_sim.py`

**Interfaces:**
- `refcalc.RefCalc(profile: str, floor: int)`: context manager; methods `reset() -> None`, `append(time: int, bits: int) -> int`, `tip(index: int) -> None`, `next_bits(time: int) -> int`, `check(time: int, bits: int, adjusted: int) -> str | None` (None means accepted, else the reject reason).
- `rules.RuleSet`: abstract, with `stamp(parent_time: int, mtp: int, adjusted: int) -> int` (what an honest miner stamps) and `accept(chain_view, time: int, adjusted: int) -> str | None`. `R0` and `R1` delegate `accept` to `RefCalc.check`. Nothing else in the sim computes bits: all `nBits` come from `RefCalc.next_bits`.
- `chain.Chain`: blocks as dataclasses `(index, parent, height, time, bits, wall_time, miner)`; methods `add`, `mtp(index)`, `tip_for(miner)`.
- `miners.Miner`: `hashrate` (relative to the target hashrate, 1.0 = difficulty-calibrated), `clock_offset` (seconds), `strategy` (`honest`).
- `sim.run(scenario: dict, rules: RuleSet, seed: int) -> Result`, where `Result` has `blocks` (list) and `metrics` (`max_stall`, `total_stall`, `max_lead`, `final_lead`, `blocks_per_hour`, `difficulty_drift`, `attacker_share`). Wall clock advances in 1s steps; each step, each miner finds a block with probability `hashrate * (1/6)` scaled by the ratio of the genesis target to the required target (use the compact-bits to target conversion; copy it from the reference rather than re-deriving difficulty rules). Seeded `random.Random(seed)`.
- Scenario JSON keys: `name`, `duration_s`, `miners` (list), `rules` (list of rule-set names), `floor`, `min_difficulty` (bool).

- [ ] **Step 1: Write failing tests** in `tests/test_sim.py`: `test_steady_state_matches_target` (R0, one honest miner at 1.0, 2 hours simulated: `blocks_per_hour` within 15% of 600); `test_high_hashrate_runs_ahead` (hashrate 3.0, 1 hour: `max_lead > 0` and `final_lead > 60`, reproducing the 260-block run's finding); `test_same_seed_identical_csv` (two runs with seed 7 give identical `blocks` serialisation); `test_different_seed_differs`.
- [ ] **Step 2:** Run `pytest contrib/timing-sim/tests/test_sim.py`. Expected: FAIL (modules missing).
- [ ] **Step 3: Implement** the modules per the Interfaces. Honest miner stamp for R0 and R1 is `max(mtp+1, parent_time + floor, adjusted)`, matching the fixed `UpdateTime()` in `src/miner.cpp`. `max_stall` counts the longest span of wall seconds during which no honest miner has a stamp that `accept` takes.
- [ ] **Step 4:** Run the tests. Expected: PASS. Run one scenario end to end: `python3 contrib/timing-sim/sim.py scenarios/steady.json` and check the summary prints R0 and R1 rows.

### Task 5: Attacks, faults, and the sim-only rule sets (R2 to R4)

**Files:**
- Modify: `contrib/timing-sim/rules.py`, `miners.py`, `sim.py`
- Create: `contrib/timing-sim/scenarios/{far_future,walk_ahead,min_diff_farm,skewed_clock,hashrate_jump}.json`, `tests/test_attacks.py`

**Interfaces:**
- Consumes: Task 4 interfaces.
- `rules.R2`, `R3`, `R4`: `accept` implemented in the sim (the only place the sim reimplements a rule; mark the block with a comment `SIM-ONLY RULE: not cross-checked against C++`). `R2`: accept iff `time > mtp` and `time <= adjusted + drift`; floor 0. `R3(window: tuple[int,int])`: R1 with floor 0 for heights inside the window. `R4(cap_multiple: int)`: R1 plus `time <= parent_time + cap_multiple * 6`. Their `nBits` still come from `RefCalc.next_bits`, called on a calculator started with floor 0 where the rule set has no floor, so LWMA sees the resulting solvetimes.
- Strategies in `miners.py`: `far_future(offset_s, at_wall_s)` (one block stamped `adjusted + offset_s`), `walk_ahead(first_s=60, step_s=15, every_s=10, drift_limit)` (as the spec says; pauses when the next stamp would exceed the rule's limit and resumes when it fits), `min_diff_farmer` (stamp `parent_time + 13` every block). `clock_offset` shifts what a miner believes is adjusted time.
- Result adds `attacker_share` (attacker blocks on the final best chain / total) and `honest_stall`.
- Rejected blocks are dropped by `sim.run` and never added to the chain or to any miner's tip.

- [ ] **Step 1: Write failing tests** in `tests/test_attacks.py`: `test_far_future_stall_r0` (one block +300s: under R0 `max_stall >= 290`; under R1 the block is rejected, so `max_stall == 0` and `attacker_share == 0`); `test_rejected_block_does_not_extend_chain` (R1 with a +100s block: no block in `blocks` has that stamp, and honest tips are unchanged); `test_walk_ahead_bounded_by_drift` (R1: `max_lead <= 72 + 6`; R0: `max_lead` grows beyond 600 in a one-hour run); `test_negative_clock_offset_no_stall_loop` (honest miner at `-120s` offset under R1 terminates and mines at least one block, with no `time-too-old` rejections); `test_min_diff_farm_gains_blocks` (testnet-style scenario with `min_difficulty=true`: farmer's blocks carry the min target and its `attacker_share` exceeds its hashrate share); `test_r2_no_stall_on_far_future` (R2: `max_stall` under 10s for the +300s block).
- [ ] **Step 2:** Run. Expected: FAIL.
- [ ] **Step 3: Implement** the three rule sets, the strategies, and the scenario files. Attack parameters come from the spec's "Calls added after the builder check": attacker hashrate swept over 0.05, 0.10, 0.20, 0.30 in `walk_ahead.json`.
- [ ] **Step 4:** Run the tests. Expected: PASS. Run all five scenarios and eyeball that R0 shows the stall and R1 does not.

### Task 6: Reporting

**Files:**
- Create: `contrib/timing-sim/report.py`, `tests/test_report.py`

**Interfaces:**
- Consumes: `sim.run` results.
- `report.table(results: dict[str, Result]) -> str` (rows = rule sets, columns = the goal-1 metrics; R2 to R4 rows end with the label `sim-only, not cross-checked against C++`), `report.write_csv(result, path)`, `report.plot_lead_and_difficulty(result, path)` (matplotlib imported lazily; skipped with a message if missing).
- CLI: `python3 contrib/timing-sim/report.py scenarios/*.json --seed 1 --out <dir>` prints a table per scenario and writes CSVs.

- [ ] **Step 1: Write failing tests**: `test_table_labels_sim_only_rows` (R2 row contains the label; R0 does not); `test_csv_round_trip` (written CSV has one row per block and the columns `height,time,wall_time,bits,miner`); `test_plot_skipped_without_matplotlib` (monkeypatch the import; the function returns without raising).
- [ ] **Step 2:** Run. Expected: FAIL.
- [ ] **Step 3: Implement** `report.py`.
- [ ] **Step 4:** Run tests. Expected: PASS. Run the CLI over all scenarios; keep the output out of the repo (scratchpad).

### Task 7: Regtest confirmation (R0 and R1)

**Files:**
- Create: `contrib/timing-sim/confirm_regtest.py`, `tests/test_confirm_smoke.py`
- Modify (only if needed): `src/rpc/mining.cpp` or none, see Step 3

**Interfaces:**
- Consumes: `-timingprofile` (Task 2), the node binaries from the Docker build, the sim's `Result` metrics for the same scenario.
- CLI: `python3 contrib/timing-sim/confirm_regtest.py --scenario far_future|walk_ahead|steady_3x --rules R0|R1 --container <name>`. It starts two regtest nodes with `-timingprofile` in the container, drives the scenario with `setmocktime` and block submission, computes `max_stall`, `max_lead` and `blocks_per_hour` from the resulting chain, runs the sim for the same scenario and seed, and prints both with a tolerance verdict. A mismatch beyond tolerance is printed as a finding and the script still exits 0 (the spec says it is not a gate). Tolerances: `max_lead` within 6s, `max_stall` within 12s, `blocks_per_hour` within 15%.
- Attack blocks with a hand-set time: build the block in Python from `getblocktemplate` data or with `generatetoaddress` after `setmocktime`, then rewrite `nTime` and re-solve (regtest proof of work is trivial) before `submitblock`. Use `setmocktime` for honest blocks. Choose whichever needs no new RPC; if neither works, add a regtest-only optional `time` argument to `generatetoaddress` in `src/rpc/mining.cpp` and say so in the report.

- [ ] **Step 1: Write the failing smoke test**: `test_confirm_far_future_r0_stalls` runs the far-future scenario under R0 on real nodes (skips if Docker or the image is missing) and asserts the measured `max_stall` is at least 290 for a +300s block, and under R1 the block is rejected (`submitblock` returns `time-too-new`).
- [ ] **Step 2:** Run it. Expected: FAIL (script missing).
- [ ] **Step 3: Implement** `confirm_regtest.py` as described.
- [ ] **Step 4:** Run the smoke test, then all three scenarios under R0 and R1, and save the printed comparison to the scratchpad. Expected: smoke test passes; scenario comparisons print verdicts.

### Task 8: Documentation

**Files:**
- Create: `contrib/timing-sim/README.md`
- Modify: `NEXT-STEPS.md` (untracked): tick the plan items this work completes and add a "Harness results" section with the first tables.

- [ ] **Step 1:** Write the README: how to build `timing_ref`, how to run the sim and the confirmation script, what each rule set means, and which rows are sim-only.
- [ ] **Step 2:** Run the full pipeline once (all scenarios, report, confirmation) and paste the summary tables into `NEXT-STEPS.md`. State in the text which conclusions the data supports about R0 to R4 and which of them has no C++ yet.
- [ ] **Step 3:** Run the full C++ suite and all Python tests one last time. Expected: all pass.
