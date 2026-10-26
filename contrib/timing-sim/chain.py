"""Synthetic chain model for the simulator.

The base block and its history match what timing_ref builds after RESET, so
that median-time-past agrees between the two.
"""
from dataclasses import dataclass
from typing import List, Optional, Tuple

BASE_TIME = 1767254400
BASE_BITS = 0x207FFFFF
HISTORY_SPACING = 6
MTP_SPAN = 11


def bits_to_target(bits: int) -> int:
    """Compact nBits to a 256-bit target (positive targets only)."""
    size = bits >> 24
    word = bits & 0x007FFFFF
    return word >> (8 * (3 - size)) if size <= 3 else word << (8 * (size - 3))


@dataclass
class Block:
    index: int
    parent: Optional[int]
    height: int
    time: int
    bits: int
    wall_time: int
    miner: str
    rc_index: int  # index of the same block inside the reference calculator


class Chain:
    def __init__(self, base_height: int, base_bits: int = BASE_BITS):
        self.base_bits = base_bits
        self.blocks: List[Block] = [Block(0, None, base_height, BASE_TIME, base_bits, 0, "base", 0)]

    def add(self, parent: Block, time: int, bits: int, wall_time: int, miner: str, rc_index: int) -> Block:
        block = Block(len(self.blocks), parent.index, parent.height + 1, time, bits, wall_time, miner, rc_index)
        self.blocks.append(block)
        return block

    def best(self) -> Block:
        """Highest block; the earliest seen wins a tie."""
        best = self.blocks[0]
        for b in self.blocks:
            if b.height > best.height:
                best = b
        return best

    def recent(self, block: Block, n: int) -> List[Tuple[int, int]]:
        """The last n (time, bits) pairs ending at `block`, oldest first, padded with the
        synthetic history below the base block (the same blocks timing_ref builds)."""
        out: List[Tuple[int, int]] = []
        cur: Optional[Block] = block
        while cur is not None and len(out) < n:
            out.append((cur.time, cur.bits))
            cur = self.blocks[cur.parent] if cur.parent is not None else None
        k = 1
        while len(out) < n:
            out.append((BASE_TIME - HISTORY_SPACING * k, self.base_bits))
            k += 1
        out.reverse()
        return out

    def mtp(self, block: Block, span: int = MTP_SPAN) -> int:
        """Median time over the block and its span - 1 ancestors."""
        times = sorted(t for t, _ in self.recent(block, span))
        return times[span // 2]
