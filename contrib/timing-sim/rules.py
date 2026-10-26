"""Rule sets. R0 and R1 delegate acceptance to the C++ reference calculator."""
from typing import Optional

import asert
import lwma
from chain import Block, Chain
from refcalc import RefCalc


class RuleSet:
    """A rule set backed by the real C++ rules through a -timingprofile."""

    drift = 7200  # R1 overrides this with the scaled-drift value

    def __init__(self, name: str, profile: str, floor: int, min_difficulty: bool = False):
        self.name = name
        self.floor = floor
        self.sim_only = False
        self.chain: Optional[Chain] = None  # set by sim.run
        self.lwma_clamp: Optional[int] = None  # sim-only: floor on solvetimes, see lwma.py
        self.lwma_monotonic = False  # sim-only: LWMA over monotonic timestamps, see lwma.py
        self.asert_tau: Optional[float] = None  # sim-only: ASERT difficulty with this half-life, see asert.py
        self.asert_wait: Optional[float] = None  # sim-only: ASERT easing per second of waiting, see asert.py
        self.asert_free = 6 * lwma.T  # sim-only: seconds after the parent before the easing starts
        self.legacy_miner = False  # True: miners stamp max(MTP+1, adjusted), ignoring the floor
        self.rc = RefCalc(profile, floor, min_difficulty)

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.rc.close()

    def floor_at(self, height: int) -> int:
        return self.floor

    def stamp(self, chain: Chain, parent: Block, adjusted: int) -> int:
        """What an honest miner stamps: the fixed UpdateTime() in src/miner.cpp,
        or the pre-fix version (legacy_miner) that ignored the spacing floor."""
        if self.legacy_miner:
            return max(chain.mtp(parent) + 1, adjusted)
        return max(chain.mtp(parent) + 1, parent.time + self.floor_at(parent.height + 1), adjusted)

    def latest_stamp(self, parent: Block, adjusted: int) -> int:
        """The latest timestamp these rules accept, for attackers that stamp as far ahead as allowed."""
        return adjusted + self.drift

    def next_bits(self, parent: Block, time: int) -> int:
        if self.asert_tau is not None:  # SIM-ONLY difficulty rule, anchored at the chain's base block
            anchor = self.chain.blocks[0]
            return asert.next_bits(parent.time, parent.height, anchor.time, anchor.height, anchor.bits,
                                   self.asert_tau, candidate_time=time, tau_wait_s=self.asert_wait,
                                   free_wait_s=self.asert_free)
        if self.lwma_clamp is not None or self.lwma_monotonic:  # SIM-ONLY LWMA variants
            return lwma.next_bits(self.chain.recent(parent, lwma.N + 1), min_solvetime=self.lwma_clamp,
                                  monotonic=self.lwma_monotonic)
        self.rc.tip(parent.rc_index)
        return self.rc.next_bits(time)

    def accept(self, chain: Chain, parent: Block, time: int, bits: int, adjusted: int) -> Optional[str]:
        self.rc.tip(parent.rc_index)
        return self.rc.check(time, bits, adjusted)

    def append(self, parent: Block, time: int, bits: int) -> int:
        self.rc.tip(parent.rc_index)
        return self.rc.append(time, bits)


class SimOnlyRuleSet(RuleSet):
    """A rule set that exists only in the simulator.

    SIM-ONLY RULE: not cross-checked against C++. nBits still come from the real
    LWMA code (a calculator with no floor); only the timestamp acceptance and the
    honest stamp are implemented here.
    """

    def __init__(self, name: str, floor: int, min_difficulty: bool = False, lwma_clamp: Optional[int] = None):
        super().__init__(name, "whippet", 0, min_difficulty)
        self.floor = floor
        self.sim_only = True
        self.lwma_clamp = lwma_clamp

    def accept(self, chain: Chain, parent: Block, time: int, bits: int, adjusted: int) -> Optional[str]:
        if time <= chain.mtp(parent):
            return "time-too-old"
        if time > adjusted + self.drift:
            return "time-too-new"
        floor = self.floor_at(parent.height + 1)
        if floor > 0 and time < parent.time + floor:
            return "bad-time-too-early"
        return None


class R2(SimOnlyRuleSet):
    """Dogecoin-style: later than the median of the last 11 blocks, no floor, 2h drift."""

    def floor_at(self, height: int) -> int:
        return 0


class R3(SimOnlyRuleSet):
    """R1 (72s drift, floor) with the floor switched off for window = [first, last) heights."""

    drift = 72

    def __init__(self, name, floor, min_difficulty=False, window=(0, 0), drift=72):
        super().__init__(name, floor, min_difficulty)
        self.window = tuple(window)
        self.drift = drift

    def floor_at(self, height: int) -> int:
        return 0 if self.window[0] <= height < self.window[1] else self.floor


class R4(SimOnlyRuleSet):
    """R1 plus a cap on each step: time <= parent.time + cap_multiple * 6."""

    drift = 72

    def __init__(self, name, floor, min_difficulty=False, cap_multiple=6):
        super().__init__(name, floor, min_difficulty)
        self.cap = cap_multiple * 6

    def stamp(self, chain: Chain, parent: Block, adjusted: int) -> int:
        # Stamping `adjusted` after a long gap would break the cap and halt the chain.
        return min(super().stamp(chain, parent, adjusted), parent.time + self.cap)

    def latest_stamp(self, parent: Block, adjusted: int) -> int:
        return min(super().latest_stamp(parent, adjusted), parent.time + self.cap)

    def accept(self, chain: Chain, parent: Block, time: int, bits: int, adjusted: int) -> Optional[str]:
        reason = super().accept(chain, parent, time, bits, adjusted)
        if reason is None and time > parent.time + self.cap:
            return "time-too-far-from-parent"
        return reason


class R2c(R2):
    """R2 with solvetimes floored at 1s in LWMA (hypothesis 4: no negative solvetimes)."""

    def __init__(self, name, floor, min_difficulty=False):
        super().__init__(name, floor, min_difficulty, lwma_clamp=1)


class R2m(R2):
    """R2 with LWMA over monotonic timestamps (hypothesis 4b)."""

    def __init__(self, name, floor, min_difficulty=False):
        super().__init__(name, floor, min_difficulty)
        self.lwma_monotonic = True


class R5(SimOnlyRuleSet):
    """Chain-relative bound with no wall clock in validation.

    A block is rejected if its time is at or before the median of the last 11 blocks, or more
    than cap_multiple * 6s after the median of the last `window` blocks. No floor.
    Honest miners stamp min(their clock, that limit), so an outage cannot halt the chain.
    """

    def __init__(self, name, floor, min_difficulty=False, window=11, cap_multiple=12, lwma_clamp=None):
        super().__init__(name, 0, min_difficulty, lwma_clamp)
        self.window = window
        self.cap = cap_multiple * 6

    def limit(self, parent: Block, chain: Optional[Chain] = None) -> int:
        return (chain or self.chain).mtp(parent, self.window) + self.cap

    def stamp(self, chain: Chain, parent: Block, adjusted: int) -> int:
        return max(chain.mtp(parent) + 1, min(adjusted, self.limit(parent, chain)))

    def latest_stamp(self, parent: Block, adjusted: int) -> int:
        return self.limit(parent)

    def accept(self, chain: Chain, parent: Block, time: int, bits: int, adjusted: int) -> Optional[str]:
        if time <= chain.mtp(parent):
            return "time-too-old"
        if time > self.limit(parent, chain):
            return "time-too-far-ahead-of-median"
        return None


class R5c(R5):
    """R5 with the solvetime floor of R2c."""

    def __init__(self, name, floor, min_difficulty=False, **params):
        super().__init__(name, floor, min_difficulty, lwma_clamp=1, **params)


class R5m(R5):
    """R5 with LWMA over monotonic timestamps."""

    def __init__(self, name, floor, min_difficulty=False, **params):
        super().__init__(name, floor, min_difficulty, **params)
        self.lwma_monotonic = True


class R1a(R3):
    """R1's acceptance rules (floor, 72s drift, median) with ASERT difficulty."""

    def __init__(self, name, floor, min_difficulty=False, tau_s=1728, tau_wait_s=None, free_wait_s=36):
        super().__init__(name, floor, min_difficulty, window=(0, 0))
        self.asert_tau = tau_s
        self.asert_wait = tau_wait_s
        self.asert_free = free_wait_s


class R2a(R2):
    """R2's acceptance rules (median only, no floor) with ASERT difficulty."""

    def __init__(self, name, floor, min_difficulty=False, tau_s=1728, tau_wait_s=None, free_wait_s=36):
        super().__init__(name, floor, min_difficulty)
        self.asert_tau = tau_s
        self.asert_wait = tau_wait_s
        self.asert_free = free_wait_s


class R6(R2a):
    """R2's acceptance rules (median only, no floor) with the 72s drift cap and ASERT difficulty."""

    drift = 72

    def __init__(self, name, floor, min_difficulty=False, tau_s=300, tau_wait_s=None, drift=72, free_wait_s=36):
        super().__init__(name, floor, min_difficulty, tau_s, tau_wait_s, free_wait_s)
        self.drift = drift


class R5a(R5):
    """R5's chain-relative bound with ASERT difficulty."""

    def __init__(self, name, floor, min_difficulty=False, tau_s=1728, tau_wait_s=None, free_wait_s=36, **params):
        super().__init__(name, floor, min_difficulty, **params)
        self.asert_tau = tau_s
        self.asert_wait = tau_wait_s
        self.asert_free = free_wait_s


def make_rules(name: str, floor: int = 4, min_difficulty: bool = False, **params) -> RuleSet:
    if name == "R0":
        return RuleSet("R0", "whippet", floor, min_difficulty)
    if name == "R1":
        rs = RuleSet("R1", "scaled-drift", floor, min_difficulty)
        rs.drift = 72  # 12 block times, matching the scaled-drift profile
        return rs
    if name == "R2":
        return R2("R2", floor, min_difficulty)
    if name == "R2c":
        return R2c("R2c", floor, min_difficulty)
    if name == "R1x":  # R1's acceptance in Python with a chosen drift; R1x(drift=72) must equal R1
        return R3("R1x", floor, min_difficulty, window=(0, 0), **params)
    if name == "R1a":
        return R1a("R1a", floor, min_difficulty, **params)
    if name == "R2a":
        return R2a("R2a", floor, min_difficulty, **params)
    if name == "R6d":  # R6 with easing while waiting for a block
        params.setdefault("tau_wait_s", 60)
        return R6("R6d", floor, min_difficulty, **params)
    if name == "R6":
        return R6("R6", floor, min_difficulty, **params)
    if name == "R5a":
        return R5a("R5a", floor, min_difficulty, **params)
    if name == "R2m":
        return R2m("R2m", floor, min_difficulty)
    if name == "R5m":
        return R5m("R5m", floor, min_difficulty, **params)
    if name == "R5":
        return R5("R5", floor, min_difficulty, **params)
    if name == "R5c":
        return R5c("R5c", floor, min_difficulty, **params)
    if name == "R3":
        return R3("R3", floor, min_difficulty, **params)
    if name == "R4":
        return R4("R4", floor, min_difficulty, **params)
    raise ValueError(f"unknown rule set {name}")
