"""R1e: R1's acceptance (floor, 72s cap) with LWMA plus easing while waiting (sim-only)."""
import json
from pathlib import Path

import sim
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
