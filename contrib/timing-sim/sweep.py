"""Parameter sweeps over rule sets and scenarios.

    python3 sweep.py sweeps/soft_fork.json --jobs 8 --out DIR

A sweep file lists configs (a rule set and its parameters), scenarios (a scenario file, optional
overrides, and the one metric to show) and a number of seeds. Every config runs against every
scenario and seed. The table shows the median over seeds with the worst case in brackets; every
individual run is also written to a CSV.
"""
import argparse
import csv
import json
import os
from concurrent.futures import ProcessPoolExecutor
from pathlib import Path
from typing import Dict, List

import sim

HERE = Path(__file__).resolve().parent


def apply_override(scenario: dict, override: Dict[str, object]) -> dict:
    """Copy of `scenario` with overrides applied: 'miner.field' sets a miner field, else a top-level key."""
    out = json.loads(json.dumps(scenario))
    for key, value in override.items():
        if "." in key:
            miner, field = key.split(".", 1)
            next(m for m in out["miners"] if m["name"] == miner)[field] = value
        else:
            out[key] = value
    return out


def expand_jobs(spec: dict) -> List[dict]:
    return [{"config": c["label"], "scenario": s["label"], "seed": seed}
            for c in spec["configs"] for s in spec["scenarios"] for seed in range(1, spec["seeds"] + 1)]


def columns_of(spec: dict) -> List[dict]:
    """The table columns: one per scenario metric (a scenario may list several under `columns`)."""
    out = []
    for s in spec["scenarios"]:
        cols = s.get("columns") or [{"label": s["label"], "metric": s["metric"], "worst": s.get("worst")}]
        for c in cols:
            out.append({"label": c["label"], "scenario": s["label"], "metric": c["metric"], "worst": c.get("worst")})
    return out


def write_rows(rows: List[dict], stem: str) -> None:
    """Save every run: raw JSON first (cannot fail on odd rows), then a CSV over all fields seen."""
    with open(stem + ".json", "w") as f:
        json.dump(rows, f)
    fields: List[str] = []
    for r in rows:
        fields += [k for k in r if k not in fields]
    with open(stem + ".csv", "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=fields)
        w.writeheader()
        w.writerows(rows)


def run_job(args):
    spec, job = args
    config = next(c for c in spec["configs"] if c["label"] == job["config"])
    scen = next(s for s in spec["scenarios"] if s["label"] == job["scenario"])
    base = json.loads((HERE / "scenarios" / scen["file"]).read_text())
    sc = apply_override(base, scen.get("override", {}))
    sc.setdefault("rule_params", {})[config["rule"]] = config.get("params", {})
    return {**job, **sim.run_scenario(sc, config["rule"], job["seed"]).metrics}


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("spec")
    ap.add_argument("--jobs", type=int, default=8)
    ap.add_argument("--out", required=True)
    args = ap.parse_args()
    spec = json.loads(Path(args.spec).read_text())
    jobs = expand_jobs(spec)
    print(f"{len(jobs)} runs ({len(spec['configs'])} configs x {len(spec['scenarios'])} scenarios x {spec['seeds']} seeds)", flush=True)
    with ProcessPoolExecutor(args.jobs) as pool:
        rows = list(pool.map(run_job, [(spec, j) for j in jobs]))
    os.makedirs(args.out, exist_ok=True)
    write_rows(rows, os.path.join(args.out, Path(args.spec).stem))

    columns = columns_of(spec)
    print(f"\n{'config':<26}" + "".join(f"{c['label']:>16}" for c in columns))
    for config in spec["configs"]:
        cells = ""
        for c in columns:
            vals = sorted(r[c["metric"]] for r in rows if r["config"] == config["label"] and r["scenario"] == c["scenario"])
            median = vals[(len(vals) - 1) // 2]
            worst = vals[0] if c["worst"] == "min" else vals[-1]
            fmt = (lambda v: f"{v:.2f}") if c["metric"] == "attacker_share" else (lambda v: f"{v:.0f}")
            cells += f"{fmt(median) + ' (' + fmt(worst) + ')':>16}"
        print(f"{config['label']:<26}{cells}", flush=True)


if __name__ == "__main__":
    main()
