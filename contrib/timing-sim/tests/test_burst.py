"""Burst-and-leave: a 100x miner for 300s, then gone. Characterisation tests (written after the
exploration, over seeds 1-3; multi_seed.py gives the fuller picture)."""
import json
from pathlib import Path

import sim

SCENARIOS = Path(__file__).resolve().parent.parent / "scenarios"


def result(file, rules, seed):
    scenario = json.loads((SCENARIOS / file).read_text())
    return sim.run_scenario(scenario, rules, seed)


def burst_blocks(res):
    return sum(1 for b in res.blocks if b[4] == "burst")


def test_floor_and_drift_cap_bound_the_size_of_a_burst():
    # With a 4s floor and a 72s drift cap the chain clock can only run ahead of real time by 72s,
    # so a burst gets about (300 + 72) / 4 blocks; without a floor it mines several times as many.
    assert burst_blocks(result("burst_pump.json", "R1a", 1)) <= 100
    assert burst_blocks(result("burst_pump.json", "R6", 1)) >= 300


def test_waiting_decay_shortens_the_wait_after_the_burst():
    for seed in (1, 2, 3):
        r6 = result("burst_pump.json", "R6", seed).metrics["post_burst_gap"]
        r6d = result("burst_pump.json", "R6d", seed).metrics["post_burst_gap"]
        assert r6d <= r6 and r6d <= 200, f"seed {seed}: R6 {r6}, R6d {r6d}"


def test_median_only_rules_with_lwma_have_a_gap_of_nearly_an_hour_after_a_pump_burst():
    # The pre-AuxPoW Pepecoin failure: no floor, so the burst compresses timestamps and LWMA makes
    # the target far harder than the remaining hashrate can meet. The run is 5400s long, so a gap
    # that reaches the end of the run is a lower bound (seeds 1-5 give 3198 to 4732s).
    for seed in (1, 2, 3):
        assert result("burst_pump.json", "R2", seed).metrics["max_gap"] >= 3000
