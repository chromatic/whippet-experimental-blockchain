# Timing harness: design

Date: 2026-10-26. Branch `feature/testnet`. Background and measurements are in `NEXT-STEPS.md` (untracked, repo root; the builder will be given a copy).

## Intent

Decide how to fix the "far-future block" problem, and what else in the block-timestamp rules needs to change with it, from measurements instead of intuition. We build a harness that runs candidate rule sets against a list of faults and attacks and reports what each does to the chain.

Why now: a tester mined one block stamped 5 minutes ahead and halted every miner for 5 minutes plus the spacing floor. Our own 260-block testnet run showed two related effects: timestamps outrun the wall clock when hashrate exceeds target, and LWMA does not respond because the spacing floor pins solvetimes at the target. A third report describes a "walk-ahead" attack that is claimed to capture the network with under 20% of the hashrate. We do not yet know how these interact, so we cannot pick a fix safely.

## Goals

1. For each combination of rule set and scenario, report:
   - maximum and total stall (seconds in which no honest miner can produce a valid block);
   - difficulty drift (change in target against an honest baseline) and the time to recover;
   - timestamp lead (chain tip time minus wall-clock time), maximum and at the end;
   - blocks per real hour, against the 6s target.
2. Cover these scenarios at minimum:
   - one block stamped far ahead (the tester's 5-minute case);
   - the walk-ahead attack (first block +1 minute, then a block +15s ahead of its parent every 10s, pause near the drift limit, repeat);
   - farming the testnet min-difficulty rule (stamp 13s or more after the parent);
   - a node or miner with a skewed clock (both directions, and a victim whose adjusted time is shifted by peers);
   - hashrate jumps up and down;
   - steady honest mining at several multiples of target hashrate.
3. Cover these rule sets at minimum (R2 to R4 are sim-only; see "Decided after the builder check"):
   - R0: today's rules (2-hour drift, floor 6s then 4s, LWMA);
   - R1: scaled drift (about 12 block times, 72s) with the floor kept;
   - R2: Dogecoin-style, no floor, median-time-past only;
   - R3: a no-floor window over a height range (a peer's suggestion), with the 72s drift;
   - R4: R1 plus a relative cap on each step from the parent.
4. Run the real consensus code under each rule set on regtest nodes, so the numbers come from the code that would ship.
5. Be repeatable: the same seed gives the same result, and a run finishes in minutes.

## Non-goals and anti-goals

- No consensus change on mainnet or public testnet. The harness measures; the drift change is a later, separate piece of work.
- No attempt to model network topology, orphaning races, or selfish mining beyond what the scenarios above need.
- Not a failure: a sim that agrees with itself but not with the C++ code. A sim result that disagrees with the real nodes counts as a bug in the sim.
- Not a failure: a rule set that loses. Showing why R0 and R2 fail is the point.

## Constraints

- The public testnet must not be used. Regtest is isolated and has trivial proof of work.
- Default regtest behaviour must not change. The existing QA tests (`qa/rpc-tests`) depend on regtest's 1s spacing, no floor, and no retargeting.
- Native builds fail on this host; build and run inside the `whippet-ubuntu24` image (see `NEXT-STEPS.md`).
- Python 3. The repo's tooling (`qa/rpc-tests`, `contrib/mining`) is Python.
- The sim must not reimplement the difficulty or header rules.

## Decisions made with the user

- Simulator first, then confirm on real nodes. Real nodes are for the top two or three scenarios only.
- Confirmation runs on regtest, via a new opt-in startup argument `-timingprofile` that makes regtest run Whippet's timing rules. The profile also has values for the candidate rule sets.

## Design

### 1. `-timingprofile` (regtest only)

A startup argument read in `CRegTestParams`. Unset means current regtest, unchanged. Accepted values: `whippet` (R0) and `scaled-drift` (R1). R2 to R4 have no profile until one of them is chosen for C++. A profile sets:

- `nPowTargetSpacing` = 6 and `nMinBlockSpacing` = 6;
- `fLWMADifficultyCalculation` = true from a low height, `fPowNoRetargeting` = false, and the LWMA window as on testnet (240 blocks);
- the proof-of-work limit stays trivial so blocks mine instantly;
- a maximum future drift, once `nMaxFutureDrift` exists in `Consensus::Params`. Until `nMaxFutureDrift` exists, R1 cannot differ from R0 on the real nodes. Adding it is therefore the first consensus-side step and is part of this work, regtest-only through the profile.

Rejects an unknown value at startup. Rejected on any network other than regtest.

### 2. Reference calculator (the sim's link to the C++ code)

A small test-only binary, or a hidden mode of `test_whippet`, that reads a chain description (heights, times, bits, nonce irrelevant) on stdin and answers two questions per candidate next block: "what `nBits` is required" (`GetNextWorkRequired`) and "is the header acceptable at adjusted time T" (`ContextualCheckBlockHeader`'s timestamp rules). Both are called on synthetic `CBlockIndex` chains, so the sim never re-implements them. Because both live in static code reached through `validation.cpp`, this may require a small refactor to expose the timestamp checks as a function. That refactor is allowed, behaviour-neutral, and covered by the existing suite.

### 3. Simulator

Python, `contrib/timing-sim/`. Event loop over simulated seconds (no real sleeping):

- Miners: hashrate, clock offset, strategy (honest, far-future, walk-ahead, min-difficulty farmer). Block discovery is a Poisson process with rate = hashrate / required target. An honest miner stamps `max(MTP+1, parent.time + floor, adjusted time)`.
- Each candidate block is passed to the reference calculator for `nBits` and for acceptance. Rejected blocks do not extend the chain.
- Outputs per run: CSV of per-block data (height, time, wall time, bits, miner), and a one-line summary of the goal-1 metrics.
- Scenario and rule-set definitions are data files, so adding a case does not need code changes.

Report: a table per scenario (rows = rule sets, columns = the goal-1 metrics). Optional plots of timestamp lead and difficulty against time for any scenario. Pass/fail thresholds are not in v1 (my call; add once the first tables show what is achievable).

### 4. Confirmation on regtest

For the chosen scenarios, a script starts two or three regtest nodes with the matching `-timingprofile`, drives the scenario with `setmocktime` and `generatetoaddress`, and compares the goal-1 metrics with the simulator's for the same scenario. A mismatch beyond a stated tolerance fails the run. Honest-miner behaviour on the real nodes depends on the `UpdateTime()` fix in `ab4b3c8eb`.

Attack blocks need hand-set timestamps. If no RPC allows that, add a regtest-only way to submit a block built with a given time (for example a `time` argument to a regtest-only generate variant, or building the block in Python and submitting with `submitblock`). Left to the builder.

## Left to the builder

- Directory and file names inside `contrib/timing-sim/`.
- The exact refactor to expose the timestamp checks, and whether the reference calculator is a new binary or a mode of `test_whippet`.
- Network model detail (latency, orphaning) beyond the scenarios above.
- The tolerance for sim versus real-node agreement.
- Plot library (optional output).

## My calls (not discussed with the user)

- Document location: `doc/superpowers/specs/` rather than `docs/`, because the repo already has `doc/`.
- Output is tables plus optional plots, with no pass/fail thresholds in v1.
- The sim lives in `contrib/timing-sim/` and the reference calculator reuses the unit-test build.
- Rule set R4 (relative step cap) is included although the notes mark it as a possible second layer.
- LWMA window is 10 blocks (`nPowTargetTimespan = 60`), as testnet's LWMA branch; LWMA starts at height 240 on testnet but the profile enables it everywhere. Corrected during implementation: the first draft said 240.
- Below height 145000 the code retargets only every `DifficultyAdjustmentInterval()` (10) blocks; from 145000 every block (`pow.cpp`). Mainnet is in the every-block regime. The simulator's reference calculator therefore starts synthetic chains at height 145000; real regtest nodes run in the 10-block regime at low heights, which affects only the difficulty metrics, not stall or lead.

## Calls added after the builder check (my calls, to be checked)

- **Honest miners during the walk-ahead attack** stamp `max(MTP+1, parent.time + floor, adjusted time)`, as the fixed `UpdateTime()` does, so they inherit the lead and stall only when a block would exceed the drift limit. Attacker hashrate is swept (5%, 10%, 20%, 30%). "Capture" is measured as the attacker's share of blocks and the honest miners' total stall; add both to the goal-1 metrics.
- **Attack parameters** come from the peer's description in `NEXT-STEPS.md`: first block +60s, then each block +15s over its parent, one every 10s of wall time, pause when the lead reaches the drift limit.
- **Floor value:** 6s up to height 100000, 4s from there (`minSpacingConsensus`, `chainparams.cpp:334`). Mainnet today runs at 4s. The profile takes the floor as a scenario variable and defaults to 4s.
- **Min-difficulty rule:** off by default in the profiles; a scenario variant turns it on, since it is a testnet-only rule.
- **R3 and R4 values:** swept, not fixed. R3 is on over a height range given in the scenario file; R4's cap is a multiple of the target spacing, swept over 2x, 6x, 12x.
- **Adjusted time** is passed to the reference calculator as a parameter. The peer-offset clamp (70 minutes) is modelled in the sim only. Real nodes use `setmocktime` and do not cover peer-offset skew.
- **Confirmation scenarios** are fixed: one far-future block, the walk-ahead attack, and steady mining at 3x target hashrate. A failed confirmation is a reported finding, not a gate.
- **Where things run:** the Python sim runs on the host and talks to a persistent reference-calculator process in the Docker image over stdin/stdout (`docker exec -i`), so the sim's throughput is not dominated by process start-up.

## Decided after the builder check

- R2 (MTP only, no floor), R3 (no-floor window) and R4 (relative step cap) are **sim-only**. The only new consensus code is `nMaxFutureDrift` (needed for R1), gated to regtest through `-timingprofile`. Any other rule set is built in C++ only if the sim shows it wins, as separate work afterwards.
- Consequence: the "sim never reimplements the rules" guarantee covers R0 and R1 only. For R2 to R4 the sim has to implement the changed rule itself, in one clearly marked place, and the report labels those rows "sim-only, not cross-checked against C++".
- Real-node confirmation therefore covers R0 and R1 only.

## Open

None. Awaiting the user's approval of this document.

## Addendum (2026-10-26, during implementation)

- Added rule sets at the user's request: R2c, R2m (R2 with an LWMA variant), R5 (a chain-relative bound with no wall clock: reject a block more than `cap_multiple` x 6s after the median of the last `window` blocks), R5c and R5m. All sim-only. A Python LWMA port (`lwma.py`) supports the variants and is checked against the C++ code.
- Added a regtest `-timingmindiff` argument (needed for the min-difficulty scenario) and a `legacy_miner` scenario flag (reproduces the pre-fix miner).
- Corrected: the LWMA window is 10 blocks, not 240; below height 145000 the code retargets only every 10 blocks, so synthetic chains start at 145000.
- The real-node confirmation covers `far_future` and `walk_ahead` for R0 and R1; the planned steady-state 3x scenario was dropped because regtest proof of work is trivial and cannot replay difficulty feedback, and the walk-ahead replay is limited to 1200s for the same reason.
- Added ASERT-style difficulty (`asert.py`; sim-only; floats, so a consensus version would need integer arithmetic) and the rule sets R1a, R2a, R5a (R1, R2, R5 acceptance with ASERT) and R6 (median only, 72s drift cap, ASERT), at the user's request.
- Added the burst-and-leave scenario (the Pepecoin pre-AuxPoW problem): `pump` strategy, `max_per_second`, `max_gap` / `post_burst_gap` metrics, `multi_seed.py`, and the R6d easing term, at the user's request. Results are in `NEXT-STEPS.md`.
