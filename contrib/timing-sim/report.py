"""Tables, CSV and plots for simulator results.

    python3 report.py scenarios/*.json --seed 1 --out <dir>
"""
import argparse
import csv
import json
import os
import re
from typing import Dict

from chain import BASE_TIME
from sim import Result, expand_scenarios, run_scenario

SIM_ONLY = {"R2", "R2c", "R2m", "R3", "R4", "R5", "R5c", "R5m", "R1a", "R2a", "R5a", "R6", "R6d", "R1x"}
SIM_ONLY_LABEL = "sim-only, not cross-checked against C++"
COLUMNS = [("max_stall", "max stall s"), ("total_stall", "total stall s"), ("max_lead", "max lead s"),
           ("final_lead", "final lead s"), ("max_gap", "max gap s"), ("blocks_per_hour", "blocks/h"),
           ("difficulty_drift", "target ratio"), ("attacker_share", "attacker share")]


def table(results: Dict[str, Result]) -> str:
    """One row per rule set; sim-only rows carry a label."""
    header = f"{'rules':<6}" + "".join(f"{title:>15}" for _, title in COLUMNS) + f"{'rejected':>10}"
    lines = [header, "-" * len(header)]
    for name, res in results.items():
        cells = "".join(f"{res.metrics[key]:>15.3f}" if isinstance(res.metrics[key], float)
                        else f"{res.metrics[key]:>15}" for key, _ in COLUMNS)
        line = f"{name:<6}{cells}{res.rejected:>10}"
        if name in SIM_ONLY:
            line += f"  ({SIM_ONLY_LABEL})"
        lines.append(line)
    return "\n".join(lines)


def write_csv(result: Result, path: str) -> None:
    with open(path, "w", newline="") as f:
        w = csv.writer(f)
        w.writerow(["height", "time", "wall_time", "bits", "miner"])
        for height, time, wall, bits, miner in result.blocks:
            w.writerow([height, time, wall, f"{bits:08x}", miner])


def plot_lead_and_difficulty(result: Result, path: str) -> bool:
    """Plot timestamp lead and target against wall time. Returns False if matplotlib is missing."""
    try:
        import matplotlib
        matplotlib.use("Agg")
        import matplotlib.pyplot as plt
    except ImportError:
        print("matplotlib is not installed; skipping plot")
        return False
    walls = [b[2] for b in result.blocks]
    lead = [b[1] - (BASE_TIME + b[2]) for b in result.blocks]
    fig, ax = plt.subplots(figsize=(8, 3))
    ax.plot(walls, lead)
    ax.set_xlabel("wall time (s)")
    ax.set_ylabel("tip time - wall time (s)")
    fig.tight_layout()
    fig.savefig(path)
    plt.close(fig)
    return True


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("scenarios", nargs="+")
    ap.add_argument("--seed", type=int, default=1)
    ap.add_argument("--out", required=True, help="directory for CSV (and plots)")
    ap.add_argument("--plots", action="store_true")
    args = ap.parse_args()
    os.makedirs(args.out, exist_ok=True)
    for path in args.scenarios:
        with open(path) as f:
            base = json.load(f)
        for label, sc in expand_scenarios(base):
            results = {}
            for rule in sc.get("rules", ["R0", "R1"]):
                res = run_scenario(sc, rule, args.seed)
                results[rule] = res
                stem = re.sub(r"[^A-Za-z0-9_.-]+", "_", f"{label}_{rule}")
                write_csv(res, os.path.join(args.out, stem + ".csv"))
                if args.plots:
                    plot_lead_and_difficulty(res, os.path.join(args.out, stem + ".png"))
            print(f"\n== {label}\n{table(results)}")


if __name__ == "__main__":
    main()
