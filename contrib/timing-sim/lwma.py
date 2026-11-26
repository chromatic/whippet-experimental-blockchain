"""Python LWMA used for sim-only rule sets that change how solvetimes are measured.

A line-for-line port of the LWMA branch of CalculateWhippetNextWorkRequired()
(src/whippet.cpp), quirks included: the newest block gets the smallest weight unless
`newest_heaviest` (the weights from the easing fork, Params::fLWMANewestHeaviest),
solvetimes below -6T are clamped to -6T, and a non-positive sum holds the previous target
(the 1.4.0-dev fix). `legacy_negative` restores the code before that fix, where a negative sum
became a huge uint32 multiplier; the sim-only rule sets use it to keep their published results.
tests/test_lwma.py checks it against the C++ code. `min_solvetime` is the only
difference from C++: solvetimes below it are raised to it before the C++ clamps.
`monotonic` is another: each timestamp in the window is first raised to the previous one plus 1
(as LWMA-1 does), so out-of-order stamps cannot break the telescoping of the weighted sum.
"""
from typing import List, Optional, Tuple

from chain import bits_to_target

POW_LIMIT = (1 << 255) - 1  # regtest powLimit
N = 10                      # LWMA window = nPowTargetTimespan 60 / spacing 6
T = 6


def target_to_bits(target: int) -> int:
    """arith_uint256::GetCompact()."""
    size = (target.bit_length() + 7) // 8
    compact = target << (8 * (3 - size)) if size <= 3 else target >> (8 * (size - 3))
    compact &= 0xFFFFFFFF
    if compact & 0x00800000:
        compact >>= 8
        size += 1
    return compact | (size << 24)


def ease_target(target: int, extra: int, tau: int, pow_limit: int = POW_LIMIT) -> int:
    """EaseTarget() in src/whippet.cpp: target * 2 ** (extra / tau), integer arithmetic, clamped to pow_limit."""
    if extra <= 0 or tau <= 0:
        return target
    if extra // tau > 255:
        return pow_limit
    x = (extra << 16) // tau
    shifts, frac = x >> 16, x & 0xFFFF
    factor = 65536 + ((195766423245049 * frac + 971821376 * frac * frac + 5127 * frac ** 3 + (1 << 47)) >> 48)
    target = min(target, pow_limit)  # as in C++: keeps every product below 2**256
    eased = (target >> 16) * factor if target.bit_length() > 239 else (target * factor) >> 16
    if shifts and eased.bit_length() + shifts > 256:
        return pow_limit
    return min(eased << shifts, pow_limit)


def next_bits(recent: List[Tuple[int, int]], min_solvetime: Optional[int] = None,
              monotonic: bool = False, candidate_time: Optional[int] = None,
              tau_wait_s: Optional[float] = None, free_wait_s: int = 6 * T,
              newest_heaviest: bool = False, legacy_negative: bool = False) -> int:
    """Required nBits for the block after `recent` ((time, bits) pairs, oldest first).

    With tau_wait_s the target is also multiplied by 2 ** ((gap - free_wait_s) / tau_wait_s), where
    gap is candidate_time minus the parent's time (nothing while gap <= free_wait_s): a chain whose
    big miner has left gets easier the longer it waits. The same term as asert.py.
    """
    parent_time = recent[-1][0]
    if monotonic:
        fixed = [recent[0]]
        for time, bits in recent[1:]:
            fixed.append((max(time, fixed[-1][0] + 1), bits))
        recent = fixed
    k = N * (N + 1) * T // 2
    sum_target = 0
    sum_time = 0
    j = 0
    for i in range(N):
        if len(recent) < i + 2:
            break
        time, bits = recent[-1 - i]
        solvetime = time - recent[-2 - i][0]
        if min_solvetime is not None and solvetime < min_solvetime:
            solvetime = min_solvetime
        if solvetime > 6 * T:
            solvetime = 6 * T
        if solvetime < -6 * T:
            solvetime = T // 6 if legacy_negative else -6 * T
        j += 1
        sum_time += solvetime * (N - j + 1 if newest_heaviest else j)
        sum_target += bits_to_target(bits) // (k * N)
    if j < N - 1:
        return recent[-1][1]
    if legacy_negative:
        next_target = (sum_target * (sum_time % (1 << 32))) % (1 << 256)
    elif sum_time <= 0:
        next_target = bits_to_target(recent[-1][1])  # no usable pace signal: hold the previous target
    else:
        next_target = min(sum_target * sum_time, POW_LIMIT)  # C++ saturates rather than wrapping 2**256
    if tau_wait_s is not None and candidate_time is not None:
        next_target = ease_target(next_target, candidate_time - parent_time - free_wait_s, int(tau_wait_s))
    if next_target > POW_LIMIT:
        next_target = POW_LIMIT
    return target_to_bits(next_target)
