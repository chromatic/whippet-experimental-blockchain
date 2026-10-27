"""The Python easing (lwma.py) and the C++ one (src/whippet.cpp, profile scaled-drift-easing) agree bit for bit."""
import random

import pytest

import lwma
from refcalc import RefCalc

BASE_TIME = 1767254400
TAU = 30


@pytest.fixture(scope="module")
def rc():
    with RefCalc("scaled-drift-easing", floor=4, easing_tau=TAU) as calc:
        yield calc


def test_easing_matches_python_on_random_chains_and_gaps(rc):
    rng = random.Random(7)
    for _ in range(40):
        rc.reset()
        recent = [(BASE_TIME - 6 * (11 - i), 0x207FFFFF) for i in range(12)]
        t = BASE_TIME
        for _ in range(rng.randint(0, 25)):
            t += rng.choice([4, 5, 6, 8, 12, 40, 90])
            bits = rc.next_bits(t)
            rc.append(t, bits)
            recent.append((t, bits))
        for gap in (4, 30, 36, 37, 50, 66, 100, 250, 600, 1500):
            cand = t + gap
            assert rc.next_bits(cand) == lwma.next_bits(
                recent[-(lwma.N + 1):], candidate_time=cand, tau_wait_s=TAU, free_wait_s=36), gap


def test_a_long_wait_eases_and_no_wait_does_not(rc):
    rc.reset(145000, 0x1E0FFFFF)
    t = BASE_TIME
    for _ in range(12):
        t += 6
        rc.append(t, rc.next_bits(t))
    quick = rc.next_bits(t + 6)
    assert rc.next_bits(t + 36) == quick       # inside the free window
    assert rc.next_bits(t + 66) != quick       # 30s past it: target doubles
