"""Miner descriptions. A miner is a plain dict in the scenario file."""
from dataclasses import dataclass, field
from typing import Optional


@dataclass
class Miner:
    name: str
    hashrate: float          # 1.0 finds a block every 6s at the base target
    clock_offset: int = 0    # seconds this miner's clock is ahead of true time
    strategy: str = "honest"
    params: dict = field(default_factory=dict)
    active_from: int = 0     # wall seconds since the start
    active_until: Optional[int] = None

    def active(self, t: int) -> bool:
        return t >= self.active_from and (self.active_until is None or t < self.active_until)


def make_miner(spec: dict) -> Miner:
    return Miner(spec["name"], spec["hashrate"], spec.get("clock_offset", 0), spec.get("strategy", "honest"),
                 spec.get("params", {}), spec.get("active_from", 0), spec.get("active_until"))
