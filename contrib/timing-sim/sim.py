"""Block-timestamp rule simulator. Run: python3 sim.py scenarios/<file>.json"""
import json
import math
import random
import sys
from dataclasses import dataclass, field
from typing import Dict, List, Tuple

from chain import BASE_TIME, Chain, bits_to_target
from miners import Miner, make_miner
from rules import RuleSet, make_rules

TARGET_SPACING = 6
# Base target for scenarios: 256x harder than regtest's powLimit, so LWMA can move both ways.
DEFAULT_BASE_BITS = 0x1F7FFFFF


@dataclass
class Result:
    blocks: List[Tuple] = field(default_factory=list)  # (height, time, wall_time, bits, miner)
    metrics: Dict[str, float] = field(default_factory=dict)
    rejected: int = 0


def run(scenario: dict, rules: RuleSet, seed: int) -> Result:
    rng = random.Random(seed)
    duration = scenario["duration_s"]
    miners: List[Miner] = [make_miner(m) for m in scenario["miners"]]
    base_bits = scenario.get("base_bits", DEFAULT_BASE_BITS)
    base_target = bits_to_target(base_bits)

    rules.legacy_miner = scenario.get("legacy_miner", False)
    rc = rules.rc
    rc.reset(scenario.get("base_height", 145000), base_bits)
    chain = Chain(scenario.get("base_height", 145000), base_bits)
    rules.chain = chain
    bits_cache: Dict[Tuple[int, int], int] = {}

    def next_bits(parent, stamp):
        key = (parent.index, stamp)
        if key not in bits_cache:
            bits_cache[key] = rules.next_bits(parent, stamp)
        return bits_cache[key]

    reference = next(m for m in miners if m.strategy == "honest")
    pending: Dict[str, bool] = {}
    stalled = 0
    max_stall = total_stall = 0
    max_lead = 0
    burst_end_wait = 0
    result = Result()

    for t in range(duration):
        now = BASE_TIME + t  # true wall-clock time
        tip = chain.best()
        if t == scenario.get("burst_end_s"):
            # How long the lone reference miner should expect to wait at the target in force now.
            stamp = rules.stamp(chain, tip, now + reference.clock_offset)
            burst_end_wait = (TARGET_SPACING * base_target / bits_to_target(next_bits(tip, stamp))
                              / reference.hashrate)
        # Can an honest miner produce a valid block right now?
        stamp = rules.stamp(chain, tip, now + reference.clock_offset)
        if rules.accept(chain, tip, stamp, next_bits(tip, stamp), now) is not None:
            stalled += 1
            total_stall += 1
            max_stall = max(max_stall, stalled)
        else:
            stalled = 0

        for m in miners:
            if not m.active(t):
                continue
            # A miner with max_per_second > 1 may find several blocks in one second (a burst).
            attempts = 1 if m.strategy == "far_future" else m.params.get("max_per_second", 1)
            for _ in range(attempts):
                tip = chain.best()
                # A pool's job (and, merge-mined, the timestamp inside it) is fixed when it is created: on a
                # new tip, then every job_refresh_s. The block carries that stamp however late it is found.
                refresh = m.params.get("job_refresh_s")
                job_t = t if not refresh else tip.wall_time + (t - tip.wall_time) // refresh * refresh
                stamp = rules.stamp(chain, tip, BASE_TIME + job_t + m.clock_offset)
                if m.strategy == "far_future":
                    if t != m.params["at_wall_s"]:
                        continue
                    stamp = now + m.params["offset_s"]
                    found = True
                else:
                    # Attackers stamp as far ahead as they want, capped at what the rules accept.
                    latest = rules.latest_stamp(tip, now + m.clock_offset)
                    if m.strategy == "walk_ahead":
                        want = (now + m.params["first_s"] if tip.time - now < m.params["first_s"]
                                else tip.time + m.params["step_s"])
                        stamp = max(stamp, min(want, latest))
                    elif m.strategy == "pump":  # earliest stamp the rules allow, to push difficulty up
                        stamp = max(chain.mtp(tip) + 1, tip.time + rules.floor_at(tip.height + 1))
                    elif m.strategy == "min_diff_farmer":
                        stamp = max(stamp, min(tip.time + 2 * TARGET_SPACING + 1, latest))
                    bits = next_bits(tip, stamp)
                    every = m.params.get("scripted_every_s")
                    if every:  # fixed schedule, retried each second after a failure (as confirm_regtest.py)
                        found = t % every == 0 or pending.get(m.name, False)
                    else:
                        rate = m.hashrate / TARGET_SPACING * bits_to_target(bits) / base_target / attempts
                        found = rng.random() < 1 - math.exp(-rate)
                if not found:
                    continue
                bits = next_bits(tip, stamp)
                if rules.accept(chain, tip, stamp, bits, now) is not None:
                    # A patient attacker holds a block the rules would refuse; only a one-shot
                    # attack counts as a rejection.
                    if m.strategy == "far_future":
                        result.rejected += 1
                    elif m.strategy == "honest":
                        result.rejected += 1
                        pending[m.name] = True
                    continue
                pending[m.name] = False
                block = chain.add(tip, stamp, bits, t, m.name, rules.append(tip, stamp, bits))
                result.blocks.append((block.height, block.time, t, bits, m.name))

        max_lead = max(max_lead, chain.best().time - now)

    walls = [0] + [b[2] for b in result.blocks] + [duration]
    max_gap = max(b - a for a, b in zip(walls, walls[1:]))
    burst_end = scenario.get("burst_end_s")
    post_burst_gap = 0
    if burst_end is not None:
        after = [b[2] for b in result.blocks if b[2] >= burst_end]
        post_burst_gap = (after[0] if after else duration) - burst_end
    best = chain.best()
    mined = best.height - chain.blocks[0].height
    result.metrics = {
        "max_stall": max_stall,
        "total_stall": total_stall,
        "max_lead": max_lead,
        "final_lead": best.time - (BASE_TIME + duration),
        "max_gap": max_gap,
        "burst_end_wait": burst_end_wait,
        "post_burst_gap": post_burst_gap,
        "blocks_per_hour": mined * 3600 / duration,
        "difficulty_drift": bits_to_target(best.bits) / base_target,
        "attacker_share": (sum(1 for b in result.blocks if b[4].startswith("attacker")) / len(result.blocks)
                           if result.blocks else 0.0),
    }
    for i, (lo, hi) in enumerate(scenario.get("windows", [])):
        in_window = sum(1 for b in result.blocks if lo <= b[2] < hi)
        result.metrics[f"window{i}_bph"] = in_window * 3600 / (hi - lo)
    return result


def expand_scenarios(scenario: dict) -> List[Tuple[str, dict]]:
    """Split a scenario with a `sweep` into one scenario per swept hashrate."""
    sweep = scenario.get("sweep")
    if not sweep:
        return [(scenario["name"], scenario)]
    out = []
    for h in sweep["hashrates"]:
        sc = json.loads(json.dumps(scenario))
        for m in sc["miners"]:
            if m["name"] == sweep["miner"]:
                m["hashrate"] = h
        out.append((f"{scenario['name']} ({sweep['miner']} {h:g})", sc))
    return out


def run_scenario(scenario: dict, rule_name: str, seed: int) -> Result:
    params = scenario.get("rule_params", {}).get(rule_name, {})
    with make_rules(rule_name, floor=scenario["floor"], min_difficulty=scenario.get("min_difficulty", False),
                    **params) as rs:
        return run(scenario, rs, seed)


if __name__ == "__main__":
    with open(sys.argv[1]) as f:
        base = json.load(f)
    seed = int(sys.argv[2]) if len(sys.argv) > 2 else 1
    for label, sc in expand_scenarios(base):
        for name in sc.get("rules", ["R0", "R1"]):
            res = run_scenario(sc, name, seed)
            print(label, name, {k: round(v, 3) for k, v in res.metrics.items()}, "rejected", res.rejected)
