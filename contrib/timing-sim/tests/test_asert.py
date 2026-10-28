"""ASERT-style difficulty: a pure function of the parent's time and height against a fixed anchor."""
import pytest

import asert
from chain import BASE_TIME, bits_to_target

ANCHOR_BITS = 0x1F7FFFFF
ANCHOR_H = 145000


def target(parent_time, parent_height, tau=600):
    bits = asert.next_bits(parent_time, parent_height, BASE_TIME, ANCHOR_H, ANCHOR_BITS, tau)
    return bits_to_target(bits)


def test_on_schedule_gives_the_anchor_target():
    on_time = BASE_TIME + 6 * 50
    assert target(on_time, ANCHOR_H + 50) == pytest.approx(bits_to_target(ANCHOR_BITS), rel=1e-6)


def test_one_tau_late_doubles_the_target():
    late = BASE_TIME + 6 * 50 + 600
    assert target(late, ANCHOR_H + 50, tau=600) == pytest.approx(2 * bits_to_target(ANCHOR_BITS), rel=1e-4)


def test_one_tau_early_halves_the_target():
    early = BASE_TIME + 6 * 50 - 600
    assert target(early, ANCHOR_H + 50, tau=600) == pytest.approx(bits_to_target(ANCHOR_BITS) / 2, rel=1e-4)


def test_target_never_exceeds_the_pow_limit():
    way_late = BASE_TIME + 10**7
    assert target(way_late, ANCHOR_H + 1) <= asert.POW_LIMIT


def test_a_single_far_future_parent_moves_the_next_target_only_slightly_for_a_long_tau():
    # The point of ASERT here: a +60s block changes the next target by 2**(60/1728), about 2.4%.
    base = target(BASE_TIME + 6 * 50, ANCHOR_H + 50, tau=1728)
    shifted = target(BASE_TIME + 6 * 50 + 60, ANCHOR_H + 50, tau=1728)
    assert shifted / base == pytest.approx(2 ** (60 / 1728), rel=1e-4)


def test_waiting_eases_the_target_beyond_six_block_times():
    # With tau_wait the target for a candidate block gets easier the longer it is since the parent:
    # nothing up to 6 block times (36s), then a doubling per tau_wait seconds.
    parent = (BASE_TIME + 6 * 50, ANCHOR_H + 50)

    def waited(gap, wait=60):
        bits = asert.next_bits(*parent, BASE_TIME, ANCHOR_H, ANCHOR_BITS, 600,
                               candidate_time=parent[0] + gap, tau_wait_s=wait)
        return bits_to_target(bits)

    assert waited(36) == waited(1)
    assert waited(36 + 60) == pytest.approx(2 * waited(36), rel=1e-4)
    assert waited(36 + 300) == pytest.approx(32 * waited(36), rel=1e-4)  # 32x stays below the pow limit


def test_without_tau_wait_the_candidate_time_is_ignored():
    parent = (BASE_TIME + 6 * 50, ANCHOR_H + 50)
    a = asert.next_bits(*parent, BASE_TIME, ANCHOR_H, ANCHOR_BITS, 600, candidate_time=parent[0] + 5000)
    b = asert.next_bits(*parent, BASE_TIME, ANCHOR_H, ANCHOR_BITS, 600)
    assert a == b
