"""Run scenarios over several seeds and report the median and the worst case per rule set.

    python3 multi_seed.py scenarios/burst_and_leave.json --seeds 5 --jobs 4

One seed hides how much a result depends on luck (for example, how long a lone honest miner
waits for a block at a high difficulty), so claims about stuck time should come from here.
"""
import argparse
import json
from concurrent.futures import ProcessPoolExecutor
from typing import Dict, List

import sim

METRICS = ["max_gap", "post_burst_gap", "blocks_per_hour", "max_lead", "total_stall"]


def summarize(values: List[float]) -> Dict[str, float]:
    ordered = sorted(values)
    return {"median": ordered[(len(ordered) - 1) // 2], "worst": ordered[-1]}


def _one(args):
    scenario, rule, seed = args
    return sim.run_scenario(scenario, rule, seed).metrics


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("scenario")
    ap.add_argument("--seeds", type=int, default=5)
    ap.add_argument("--jobs", type=int, default=4)
    args = ap.parse_args()
    with open(args.scenario) as f:
        base = json.load(f)
    for label, sc in sim.expand_scenarios(base):
        print(f"\n== {label}  ({args.seeds} seeds; median / worst)")
        print(f"{'rules':<6}" + "".join(f"{m:>26}" for m in METRICS))
        for rule in sc.get("rules", ["R0", "R1"]):
            with ProcessPoolExecutor(args.jobs) as pool:
                runs = list(pool.map(_one, [(sc, rule, seed) for seed in range(1, args.seeds + 1)]))
            cells = ""
            for metric in METRICS:
                s = summarize([r[metric] for r in runs])
                cells += f"{s['median']:>12.0f} / {s['worst']:<11.0f}"
            print(f"{rule:<6}{cells}", flush=True)


if __name__ == "__main__":
    main()
