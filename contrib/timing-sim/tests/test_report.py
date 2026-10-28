"""Report tests. These use hand-built results, so they need neither Docker nor the binary."""
import csv
import sys

import report
from chain import BASE_TIME
from sim import Result

METRICS = {"max_stall": 305, "total_stall": 12, "max_lead": 300, "final_lead": -4,
           "blocks_per_hour": 492.0, "difficulty_drift": 2.87, "attacker_share": 0.004,
           "max_gap": 41, "post_burst_gap": 0}


def result():
    return Result(blocks=[(145001, BASE_TIME + 4, 3, 0x1F7FFFFE, "honest"),
                          (145002, BASE_TIME + 10, 9, 0x1F7FFFFD, "attacker")],
                  metrics=dict(METRICS), rejected=2)


def test_table_labels_sim_only_rows():
    text = report.table({"R0": result(), "R2": result()})
    assert "max gap s" in text.splitlines()[0]
    lines = {line.split()[0]: line for line in text.splitlines() if line[:2] in ("R0", "R2")}
    assert "sim-only, not cross-checked against C++" in lines["R2"]
    assert "sim-only" not in lines["R0"]


def test_csv_round_trip(tmp_path):
    path = tmp_path / "r.csv"
    report.write_csv(result(), str(path))
    rows = list(csv.DictReader(open(path)))
    assert len(rows) == 2
    assert list(rows[0].keys()) == ["height", "time", "wall_time", "bits", "miner"]
    assert rows[1]["miner"] == "attacker" and int(rows[1]["time"]) == BASE_TIME + 10


def test_plot_skipped_without_matplotlib(monkeypatch, tmp_path, capsys):
    monkeypatch.setitem(sys.modules, "matplotlib", None)
    monkeypatch.setitem(sys.modules, "matplotlib.pyplot", None)
    assert report.plot_lead_and_difficulty(result(), str(tmp_path / "p.png")) is False
    assert "matplotlib" in capsys.readouterr().out
