"""R1e: R1's acceptance (floor, 72s cap) with LWMA plus easing while waiting (sim-only)."""
import json
from pathlib import Path

import sim
from rules import make_rules
from test_attacks import honest, run, scenario, walk_ahead

SCENARIOS = Path(__file__).resolve().parent.parent / "scenarios"


def test_r1e_matches_r1x_when_no_gap_is_long_enough_to_ease():
    # R1x takes its bits from the C++ LWMA, R1e from the Python port. With honest blocks every 6s the
    # easing never triggers (the free window is 36s), so they must agree block for block.
    sc = scenario([honest()], duration_s=900)
    sc["miners"][0]["params"] = {"scripted_every_s": 6}
    x = run(sc, rules="R1x", seed=3, drift=72)
    e = run(sc, rules="R1e", seed=3, drift=72, tau_wait_s=60)
    assert e.blocks == x.blocks and len(x.blocks) > 100


def test_a_walk_ahead_attacker_gets_easing_on_its_own_blocks():
    # The easing is keyed to the candidate's own timestamp, which the miner chooses (within the cap).
    # A block stamped 66s after its parent is more than the 36s free window late, so R1e eases its
    # target where R1x does not. This is the cost of the design, measured by the walk-ahead columns.
    sc = scenario([honest(), walk_ahead(0.3)], duration_s=600)
    x = run(sc, rules="R1x", seed=3, drift=72)
    e = run(sc, rules="R1e", seed=3, drift=72, tau_wait_s=60)
    assert e.blocks != x.blocks


def test_r1e_has_much_shorter_gaps_than_r1x_after_a_burst_leaves():
    scenario_ = json.loads((SCENARIOS / "burst_pump.json").read_text())
    scenario_["duration_s"] = 3600
    for seed in (1, 2, 3):
        x = sim.run_scenario({**scenario_, "rule_params": {"R1x": {"drift": 72}}}, "R1x", seed).metrics["max_gap"]
        e = sim.run_scenario({**scenario_, "rule_params": {"R1e": {"drift": 72, "tau_wait_s": 60}}}, "R1e", seed).metrics["max_gap"]
        assert e < x and e <= 300, f"seed {seed}: R1x {x}, R1e {e}"


def test_stale_pool_jobs_slow_the_easing_only_when_refreshed_rarely():
    # A merge-mined block carries the timestamp of the job it was mined on. With the job refreshed every
    # 30s the burst recovery is unchanged; at every 60s the easing moves in 60s steps and recovery is slower.
    base = json.loads((SCENARIOS / "burst_pump.json").read_text())
    base["duration_s"] = 3600
    base["rule_params"] = {"R1e": {"drift": 72, "tau_wait_s": 30}}

    def gap(refresh, seed):
        sc = json.loads(json.dumps(base))
        if refresh:
            next(m for m in sc["miners"] if m["name"] == "honest").setdefault("params", {})["job_refresh_s"] = refresh
        return sim.run_scenario(sc, "R1e", seed).metrics["post_burst_gap"]

    for seed in (1, 2, 3):
        assert gap(30, seed) <= gap(None, seed) + 40
        assert gap(60, seed) <= 300


def test_r1f_is_the_fork_as_coded():
    # R1f is what ships at the easing height: the fixed LWMA, the new weights, tau 30, free wait 54.
    with make_rules("R1f", floor=4) as rs:
        assert rs.lwma_newest_heaviest and not rs.lwma_legacy
        assert rs.lwma_wait == 30 and rs.asert_free == 54
    with make_rules("R1e", floor=4) as rs:
        assert not rs.lwma_newest_heaviest and rs.lwma_legacy
