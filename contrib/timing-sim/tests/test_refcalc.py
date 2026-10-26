"""Protocol tests for the timing_ref reference calculator (needs Docker and the built image)."""
import pytest

from refcalc import RefCalc, RefCalcError

BASE_TIME = 1767254400  # time of the synthetic base block after RESET


@pytest.fixture(scope="module")
def rc():
    with RefCalc("whippet", floor=4) as calc:
        yield calc


def test_next_bits_right_after_reset(rc):
    rc.reset()
    bits = rc.next_bits(BASE_TIME + 6)
    assert 0x207FFFF0 <= bits <= 0x207FFFFF


def test_check_accepts_at_floor_and_rejects_below_it(rc):
    rc.reset()
    ok_time = BASE_TIME + 4
    assert rc.check(ok_time, rc.next_bits(ok_time), ok_time) is None
    early = BASE_TIME + 3
    assert rc.check(early, rc.next_bits(early), early) == "bad-time-too-early"


def test_check_rejects_future_block_with_default_drift(rc):
    rc.reset()
    t = BASE_TIME + 4
    far = t + 7201
    assert rc.check(far, rc.next_bits(far), t) == "time-too-new"


def test_append_returns_indexes_and_tip_forks(rc):
    rc.reset()
    first = rc.append(BASE_TIME + 6, rc.next_bits(BASE_TIME + 6))
    second = rc.append(BASE_TIME + 12, rc.next_bits(BASE_TIME + 12))
    assert (first, second) == (1, 2)
    rc.tip(first)
    fork = rc.append(BASE_TIME + 18, rc.next_bits(BASE_TIME + 18))
    assert fork == 3
    # The fork's parent is `first`, so a block 4s after `first` is acceptable.
    rc.tip(first)
    t = BASE_TIME + 10
    assert rc.check(t, rc.next_bits(t), t) is None


def test_bogus_command_errors_and_process_survives(rc):
    rc.reset()
    with pytest.raises(RefCalcError):
        rc.command("BOGUS 1 2 3")
    assert rc.next_bits(BASE_TIME + 6) > 0


def test_reset_with_bits_leaves_headroom_to_ease(rc):
    rc.reset(145000, 0x1F7FFFFF)
    t = BASE_TIME
    for _ in range(12):  # slow blocks: LWMA must make the target easier than the base
        t += 24
        rc.append(t, rc.next_bits(t))
    assert rc.next_bits(t + 24) > 0x1F7FFFFF


def test_min_difficulty_block_uses_pow_limit():
    with RefCalc("whippet", floor=4, min_difficulty=True) as calc:
        calc.reset(157500, 0x1F7FFFFF)
        normal = calc.next_bits(BASE_TIME + 6)
        slow = calc.next_bits(BASE_TIME + 13)  # more than 2 * 6s after the parent
        assert normal <= 0x1F7FFFFF
        assert slow == 0x207FFFFF
