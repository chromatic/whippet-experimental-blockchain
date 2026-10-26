"""Candidate rule sets R2c (R2 with a solvetime clamp) and R5 (bound relative to the median)."""
import sim
from chain import BASE_TIME, Chain
from rules import make_rules
from test_attacks import honest, run, scenario, walk_ahead

BASE_BITS = 0x1F7FFFFF


def test_r5_accept_does_not_look_at_the_wall_clock():
    with make_rules("R5", floor=4) as rs:
        chain = Chain(145000, BASE_BITS)
        tip = chain.best()
        t = BASE_TIME + 6
        for adjusted in (0, BASE_TIME, BASE_TIME + 10**6):
            assert rs.accept(chain, tip, t, BASE_BITS, adjusted) is None


def test_r5_rejects_beyond_the_cap_over_the_median():
    with make_rules("R5", floor=4, window=11, cap_multiple=12) as rs:
        chain = Chain(145000, BASE_BITS)
        tip = chain.best()
        median = chain.mtp(tip, 11)
        assert rs.accept(chain, tip, median + 72, BASE_BITS, 0) is None
        assert rs.accept(chain, tip, median + 73, BASE_BITS, 0) == "time-too-far-ahead-of-median"
        assert rs.accept(chain, tip, median, BASE_BITS, 0) == "time-too-old"


def test_r2c_clamp_helps_r2_but_does_not_restore_the_block_rate():
    # Hypothesis 4, tested: flooring solvetimes at 1s lifts R2 from about 350 to about 470 blocks/hour
    # under the walk-ahead attack, but not back to the 580 an honest-only chain gets.
    sc = scenario([honest(), walk_ahead(0.2)], duration_s=7200)
    r2 = run(sc, rules="R2").metrics["blocks_per_hour"]
    r2c = run(sc, rules="R2c").metrics["blocks_per_hour"]
    assert r2 < 400 < r2c < 520


def test_r5_bounds_the_lead_with_no_wall_clock_check():
    sc = scenario([honest(), walk_ahead(0.3)], duration_s=3600)
    assert run(sc, rules="R5", cap_multiple=12).metrics["max_lead"] <= 72 + 30


def test_r5_resumes_after_an_outage():
    # Honest miners are off for 900s. Their stamps must be capped at median + cap, otherwise the
    # first block after the gap is refused and the chain halts for good.
    sc = scenario([honest(active_until=300), honest(active_from=1200)], duration_s=2400)
    result = run(sc, rules="R5", cap_multiple=12)
    assert sum(1 for b in result.blocks if b[2] >= 1200) >= 50


def test_monotonic_lwma_does_not_fix_it_either():
    # Hypothesis 4b, tested: monotonic timestamps inside LWMA leave R2m too slow and make R5m too
    # fast under the walk-ahead attack (0.2 hashrate, 2 hours). Out-of-order stamps are not the only cause.
    sc = scenario([honest(), walk_ahead(0.2)], duration_s=7200)
    assert run(sc, rules="R2m").metrics["blocks_per_hour"] < 500
    assert run(sc, rules="R5m").metrics["blocks_per_hour"] > 800


def test_r5_without_a_lwma_fix_decouples_chain_time_from_real_time():
    # The unclamped R5 under the same attack ends the run with its timestamps 1.9 hours behind the wall clock.
    sc = scenario([honest(), walk_ahead(0.2)], duration_s=7200)
    assert run(sc, rules="R5").metrics["final_lead"] < -3600
