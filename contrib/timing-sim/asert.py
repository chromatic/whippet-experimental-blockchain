"""ASERT-style difficulty for sim-only rule sets (not in C++).

The next target depends only on the parent's time and height against a fixed anchor:

    next_target = anchor_target * 2 ** (lateness / tau)
    lateness    = parent.time - (anchor.time + T * (parent.height - anchor.height))

The `tau_wait_s` term is the one addition: see next_bits().

A parent stamped ahead of schedule makes the next target easier by 2**(lead/tau); once a block
with a normal timestamp becomes the parent the effect is gone. There are no per-block clamps.
The compact nBits encoding quantises the result (up to 2**-16 relative). Floats are fine for a simulator; a consensus version would need integer arithmetic.
"""
from typing import Optional

from chain import bits_to_target
from lwma import POW_LIMIT, T, target_to_bits

__all__ = ["next_bits", "POW_LIMIT"]


def next_bits(parent_time: int, parent_height: int, anchor_time: int, anchor_height: int,
              anchor_bits: int, tau_s: float, candidate_time: Optional[int] = None,
              tau_wait_s: Optional[float] = None) -> int:
    """Required nBits for a block at `candidate_time`.

    With tau_wait_s, a candidate more than 6 block times after its parent also gets an easier
    target, doubling every tau_wait_s seconds: waiting for a block lowers the difficulty, so a
    chain whose big miner has left recovers without a block having to be found first.
    """
    lateness = parent_time - (anchor_time + T * (parent_height - anchor_height))
    exponent = lateness / tau_s
    if tau_wait_s is not None and candidate_time is not None:
        exponent += max(0, candidate_time - parent_time - 6 * T) / tau_wait_s
    if exponent > 300:  # 2.0 ** x would overflow; the pow limit is far below this anyway
        target = POW_LIMIT
    else:
        target = max(1, min(int(bits_to_target(anchor_bits) * 2.0 ** exponent), POW_LIMIT))
    return target_to_bits(target)
