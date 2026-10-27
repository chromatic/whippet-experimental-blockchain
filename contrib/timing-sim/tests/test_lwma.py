"""The Python LWMA must reproduce the C++ one exactly before any variant of it is trusted."""
import random

import pytest

import lwma
from chain import BASE_TIME
from refcalc import RefCalc

BASE_BITS = 0x1F7FFFFF


def history(bits=BASE_BITS):
    # The same 12 synthetic blocks timing_ref builds after RESET.
    return [(BASE_TIME - 6 * (11 - i), bits) for i in range(12)]


@pytest.mark.parametrize("seed", [1, 2, 3])
def test_matches_cpp_on_random_chains(seed):
    rng = random.Random(seed)
    with RefCalc("whippet", floor=0) as rc:
        rc.reset(145000, BASE_BITS)
        chain = history()
        t = BASE_TIME
        for _ in range(40):
            # Solvetimes include zero, negative, and larger than 6 * target (all clamped by LWMA).
            t += rng.choice([-30, -5, 0, 1, 2, 4, 6, 9, 15, 40, 90])
            expected = rc.next_bits(t)
            assert lwma.next_bits(chain) == expected, f"mismatch at block {len(chain) - 12}"
            rc.append(t, expected)
            chain.append((t, expected))


def test_min_solvetime_floors_a_negative_solvetime():
    recent = history()
    t = BASE_TIME
    for dt in [6] * 11 + [-20]:  # the newest block has the smallest weight, so the sum stays positive
        t += dt
        recent.append((t, BASE_BITS))
    unclamped = lwma.next_bits(recent)
    clamped = lwma.next_bits(recent, min_solvetime=1)
    # A negative solvetime shrinks the sum and so makes the target harder; flooring it removes that.
    assert lwma.bits_to_target(clamped) > lwma.bits_to_target(unclamped)


def test_negative_weighted_sum_gives_an_easier_target_than_the_base():
    # Old negative solvetimes carry the largest weights. The sum goes negative, wraps as a uint32,
    # and the product wraps modulo 2**256: the C++ code returns an arbitrary target (matches C++).
    recent = history()
    t = BASE_TIME
    for dt in [-20, 6, -20, 6, 6, 6, 6, 6, 6, 6, 6, 6]:
        t += dt
        recent.append((t, BASE_BITS))
    assert lwma.bits_to_target(lwma.next_bits(recent)) != lwma.bits_to_target(BASE_BITS)


def test_monotonic_lwma_makes_timestamps_increasing_before_measuring():
    # A block stamped 55s ahead followed by blocks at the normal pace: with monotonic timestamps the
    # following blocks count as 1s solvetimes, so the weighted sum tracks the elapsed virtual time.
    recent = history()
    t = BASE_TIME
    for dt in [6] * 8 + [55, -50, 6, 6]:
        t += dt
        recent.append((t, BASE_BITS))
    plain = lwma.next_bits(recent)
    mono = lwma.next_bits(recent, monotonic=True)
    assert mono != plain
    # Without any out-of-order stamp the two agree.
    ordered = history()
    t = BASE_TIME
    for _ in range(12):
        t += 6
        ordered.append((t, BASE_BITS))
    assert lwma.next_bits(ordered, monotonic=True) == lwma.next_bits(ordered)


def _steady_chain(n=12, bits=BASE_BITS):
    recent = history(bits)
    t = BASE_TIME
    for _ in range(n):
        t += 6
        recent.append((t, bits))
    return recent, t


def test_easing_is_off_within_the_free_window():
    recent, parent_time = _steady_chain()
    plain = lwma.next_bits(recent)
    for gap in (1, 6, 36):
        assert lwma.next_bits(recent, candidate_time=parent_time + gap, tau_wait_s=60) == plain


def test_easing_doubles_the_target_for_each_tau_wait_after_the_free_window():
    recent, parent_time = _steady_chain()

    def target(gap):
        return lwma.bits_to_target(lwma.next_bits(recent, candidate_time=parent_time + gap, tau_wait_s=60))

    assert target(36 + 60) == pytest.approx(2 * target(36), rel=1e-4)
    assert target(36 + 180) == pytest.approx(8 * target(36), rel=1e-4)


def test_easing_never_exceeds_the_pow_limit():
    recent, parent_time = _steady_chain()
    bits = lwma.next_bits(recent, candidate_time=parent_time + 10**7, tau_wait_s=60)
    assert lwma.bits_to_target(bits) <= lwma.POW_LIMIT
