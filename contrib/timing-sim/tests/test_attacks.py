"""Attack, fault and sim-only rule tests (need Docker and the built image)."""
import pytest

import sim
from chain import BASE_TIME
from rules import make_rules


def honest(hashrate=1.0, **kw):
    return {"name": "honest", "hashrate": hashrate, "strategy": "honest", **kw}


def scenario(miners, duration_s=1200, floor=4, base_height=145000, **extra):
    return {"name": "t", "duration_s": duration_s, "floor": floor, "base_height": base_height,
            "miners": miners, **extra}


def run(sc, rules="R0", seed=1, **rule_params):
    with make_rules(rules, floor=sc["floor"], min_difficulty=sc.get("min_difficulty", False),
                    **rule_params) as rs:
        return sim.run(sc, rs, seed)


def far_future(offset_s, at_wall_s=600):
    return {"name": "attacker", "hashrate": 0.0, "strategy": "far_future",
            "params": {"offset_s": offset_s, "at_wall_s": at_wall_s}}


def test_far_future_stalls_legacy_miners_under_r0():
    # The tester's report: miners that ignore the spacing floor halt for the lead + 4s.
    result = run(scenario([honest(), far_future(300)], legacy_miner=True))
    assert result.metrics["max_stall"] >= 290


def test_far_future_fixed_miners_inherit_the_lead_under_r0():
    # With UpdateTime() honouring the floor there is no halt, but the clock stays ahead.
    result = run(scenario([honest(), far_future(300)]))
    assert result.metrics["max_stall"] == 0
    assert result.metrics["max_lead"] >= 300


def test_far_future_rejected_under_r1():
    result = run(scenario([honest(), far_future(300)], legacy_miner=True), rules="R1")
    assert result.rejected >= 1
    assert result.metrics["attacker_share"] == 0
    # Legacy miners wait out the 4s floor after each block; the attack adds nothing to that.
    assert result.metrics["max_stall"] <= 4
    assert result.metrics["max_lead"] < 72 + 30


def test_rejected_block_does_not_extend_chain():
    result = run(scenario([honest(), far_future(100)]), rules="R1")
    assert result.rejected == 1
    assert all(b[4] != "attacker" for b in result.blocks)
    heights = [b[0] for b in result.blocks]
    assert heights == list(range(heights[0], heights[0] + len(heights)))


def test_r2_no_stall_on_far_future():
    result = run(scenario([honest(), far_future(300)], legacy_miner=True), rules="R2")
    assert result.metrics["max_stall"] < 10


def test_negative_clock_offset_still_mines_under_r1():
    result = run(scenario([honest(), {"name": "slow", "hashrate": 1.0, "clock_offset": -120,
                                      "strategy": "honest"}]), rules="R1")
    assert any(b[4] == "slow" for b in result.blocks)
    assert result.rejected == 0


def test_miner_active_window():
    result = run(scenario([honest(active_from=300, active_until=600)], duration_s=900))
    assert result.blocks and all(300 <= b[2] < 600 for b in result.blocks)


def test_r4_cap_does_not_halt_the_chain_after_a_gap():
    # The miner is off for 600s, far longer than the 36s step cap; blocks must resume.
    sc = scenario([honest(active_until=300), honest(active_from=900)], duration_s=1500)
    result = run(sc, rules="R4", cap_multiple=6)
    assert any(b[2] >= 900 for b in result.blocks)


def walk_ahead(hashrate):
    return {"name": "attacker", "hashrate": hashrate, "strategy": "walk_ahead",
            "params": {"first_s": 60, "step_s": 15}}


def test_walk_ahead_bounded_by_scaled_drift():
    sc = scenario([honest(), walk_ahead(0.3)], duration_s=3600)
    assert run(sc, rules="R1").metrics["max_lead"] <= 72


def test_walk_ahead_runs_away_under_todays_rules():
    sc = scenario([honest(), walk_ahead(0.3)], duration_s=3600)
    assert run(sc, rules="R0").metrics["max_lead"] >= 600


def test_min_diff_farm_gains_blocks():
    farmer = {"name": "attacker", "hashrate": 0.3, "strategy": "min_diff_farmer"}
    sc = scenario([honest(), farmer], duration_s=1800, base_height=157500, min_difficulty=True)
    result = run(sc)
    assert result.metrics["attacker_share"] > 0.3 / 1.3


def test_walk_ahead_adapts_to_the_step_cap():
    # The attacker stamps as far ahead as the rules allow, so a step cap does not
    # make the attack vanish; it only shortens each step.
    sc = scenario([honest(), walk_ahead(0.3)], duration_s=3600)
    assert run(sc, rules="R4", cap_multiple=6).metrics["attacker_share"] > 0.05
