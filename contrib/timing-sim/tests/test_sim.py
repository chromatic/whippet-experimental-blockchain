"""Simulator core tests: R0/R1 with honest miners (needs Docker and the built image)."""
import pytest

import sim
from rules import make_rules


def scenario(hashrate=1.0, duration_s=3600, floor=4):
    return {
        "name": "t",
        "duration_s": duration_s,
        "floor": floor,
        "base_height": 145000,
        "miners": [{"name": "honest", "hashrate": hashrate, "clock_offset": 0, "strategy": "honest"}],
    }


def run(sc, rules="R0", seed=1):
    with make_rules(rules, floor=sc["floor"]) as rs:
        return sim.run(sc, rs, seed)


def test_steady_state_matches_target():
    result = run(scenario(hashrate=1.0, duration_s=7200))
    assert result.metrics["blocks_per_hour"] == pytest.approx(600, rel=0.15)


def test_floor_equal_to_target_blinds_lwma():
    # A 6s floor equals the 6s target spacing, so every solvetime is clamped to the
    # target and LWMA never raises difficulty (the 260-block testnet run).
    result = run(scenario(hashrate=3.0, duration_s=3600, floor=6))
    assert result.metrics["max_lead"] >= 1000
    assert result.metrics["difficulty_drift"] == pytest.approx(1.0, abs=0.01)


def test_floor_below_target_lead_is_transient():
    # With a 4s floor LWMA sees solvetimes under the target and adapts: a surge
    # briefly puts the chain clock ahead, then it settles back near the wall clock.
    result = run(scenario(hashrate=10.0, duration_s=7200, floor=4))
    assert result.metrics["max_lead"] >= 30
    assert abs(result.metrics["final_lead"]) <= 20


def test_same_seed_identical_blocks():
    a = run(scenario(duration_s=900), seed=7)
    b = run(scenario(duration_s=900), seed=7)
    assert a.blocks == b.blocks and len(a.blocks) > 0


def test_different_seed_differs():
    a = run(scenario(duration_s=900), seed=7)
    b = run(scenario(duration_s=900), seed=8)
    assert a.blocks != b.blocks


def test_scripted_arrivals_every_period():
    # Used to replay the same schedule as the real-node confirmation.
    sc = scenario(duration_s=120)
    sc["miners"][0]["params"] = {"scripted_every_s": 6}
    result = run(sc)
    assert [b[2] for b in result.blocks] == list(range(0, 120, 6))


def test_max_gap_is_the_longest_wall_clock_wait_between_blocks():
    sc = scenario(duration_s=120)
    sc["miners"][0]["params"] = {"scripted_every_s": 6}
    assert run(sc).metrics["max_gap"] == 6


def test_max_gap_counts_the_wait_after_the_last_block():
    sc = scenario(duration_s=200)
    sc["miners"][0].update({"params": {"scripted_every_s": 6}, "active_until": 30})
    # last block at wall second 24 (the miner is off from second 30), so the tail wait is 176s
    assert run(sc).metrics["max_gap"] == 200 - 24


def test_a_miner_with_max_per_second_can_find_several_blocks_in_one_second():
    sc = scenario(duration_s=3)
    sc["miners"][0].update({"hashrate": 100.0, "params": {"max_per_second": 64}})
    blocks_in_first_second = [b for b in run(sc).blocks if b[2] == 0]
    assert len(blocks_in_first_second) > 5


def test_windows_report_blocks_per_hour_inside_each_window():
    sc = scenario(duration_s=600)
    sc["miners"][0]["params"] = {"scripted_every_s": 6}
    sc["windows"] = [[0, 300], [300, 600]]
    m = run(sc).metrics
    assert m["window0_bph"] == 600 and m["window1_bph"] == 600


def test_burst_end_wait_is_the_expected_honest_wait_when_the_burst_ends():
    # One honest miner at hashrate 1 and a target near the base one: about 6s, more if it got harder.
    sc = scenario(duration_s=400)
    sc["burst_end_s"] = 300
    wait = run(sc).metrics["burst_end_wait"]
    assert 3 <= wait <= 15


def test_burst_end_wait_is_zero_when_the_scenario_has_no_burst():
    assert run(scenario(duration_s=60)).metrics["burst_end_wait"] == 0


def test_start_bits_defaults_to_base_bits_and_an_easy_start_makes_the_unbounded_walk_ahead_run_away():
    # The real-node replay starts at regtest's powLimit while measuring hashrate against a target 64
    # times lower. Unbounded walk-ahead (R0) runs away from that start on every seed; from the base
    # target it only does on some. R1 holds the cap either way.
    import json
    from pathlib import Path
    sc = json.load(open(Path(__file__).resolve().parent.parent / "scenarios" / "walk_ahead.json"))
    sc["duration_s"] = 600
    plain = sim.run_scenario(sc, "R1", 1).metrics
    same = sim.run_scenario({**sc, "start_bits": sim.DEFAULT_BASE_BITS}, "R1", 1).metrics
    assert plain == same
    easy = {**sc, "base_bits": 0x20020000, "start_bits": 0x207FFFFF}
    assert all(sim.run_scenario(easy, "R0", s).metrics["max_lead"] > 72 for s in (1, 2, 3))
    assert all(sim.run_scenario(easy, "R1", s).metrics["max_lead"] == 72 for s in (1, 2, 3))
