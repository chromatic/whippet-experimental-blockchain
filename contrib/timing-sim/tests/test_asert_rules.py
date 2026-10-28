"""ASERT difficulty combined with the R1, R2 and R5 acceptance rules (sim-only)."""
import pytest

from test_attacks import honest, run, scenario, walk_ahead

TAU = 1728  # 288 blocks of 6s, the BCH half-life scaled by block time


@pytest.mark.parametrize("rules", ["R1a", "R2a", "R5a"])
def test_honest_only_block_rate_is_on_target(rules):
    bph = run(scenario([honest()], duration_s=3600), rules=rules, tau_s=TAU).metrics["blocks_per_hour"]
    assert 520 <= bph <= 680, f"{rules}: {bph}"


@pytest.mark.parametrize("rules", ["R1a", "R2a", "R5a"])
def test_walk_ahead_attacker_does_not_move_the_block_rate(rules):
    # Hypothesis: ASERT has no per-block clamps and forgets a future-stamped parent at once.
    sc = scenario([honest(), walk_ahead(0.2)], duration_s=7200)
    bph = run(sc, rules=rules, tau_s=TAU).metrics["blocks_per_hour"]
    assert 520 <= bph <= 700, f"{rules}: {bph}"


def test_long_tau_reacts_slowly_to_a_hashrate_surge():
    surge = scenario([honest(), honest(hashrate=9.0, active_from=1800, active_until=3600)], duration_s=5400)
    lwma = run(surge, rules="R1").metrics["blocks_per_hour"]
    asert = run(surge, rules="R1a", tau_s=TAU).metrics["blocks_per_hour"]
    assert asert > 1.1 * lwma


def far_future(offset_s, at_wall_s=600):
    return {"name": "attacker", "hashrate": 0.0, "strategy": "far_future",
            "params": {"offset_s": offset_s, "at_wall_s": at_wall_s}}


def test_r6_rejects_a_far_future_block_where_r2a_accepts_it():
    sc = scenario([honest(), far_future(300)], duration_s=1800)
    assert run(sc, rules="R2a", tau_s=300).metrics["max_lead"] >= 300
    r6 = run(sc, rules="R6", tau_s=300)
    assert r6.rejected >= 1 and r6.metrics["attacker_share"] == 0 and r6.metrics["max_lead"] <= 72


def test_r6_walk_ahead_is_bounded_and_the_block_rate_holds():
    sc = scenario([honest(), walk_ahead(0.2)], duration_s=7200)
    m = run(sc, rules="R6", tau_s=300).metrics
    assert m["max_lead"] <= 72 and 520 <= m["blocks_per_hour"] <= 700
