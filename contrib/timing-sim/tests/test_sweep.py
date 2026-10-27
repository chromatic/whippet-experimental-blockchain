"""Parameterised rule sets and the sweep driver."""
import pytest

import asert
import sim
import sweep
from chain import BASE_TIME, bits_to_target
from rules import make_rules
from test_attacks import honest, run, scenario, walk_ahead


def test_r1x_at_72s_reproduces_the_cpp_r1_exactly():
    # R1x is R1's acceptance written in Python; with the same drift it must make the same decisions
    # as the real C++ code, block for block, including every rejection.
    sc = scenario([honest(), walk_ahead(0.3)], duration_s=2400)
    cpp = run(sc, rules="R1", seed=3)
    py = run(sc, rules="R1x", seed=3, drift=72)
    assert py.blocks == cpp.blocks and py.rejected == cpp.rejected and cpp.rejected > 0


def test_r1x_drift_changes_the_lead_bound():
    sc = scenario([honest(), walk_ahead(0.3)], duration_s=1800)
    assert run(sc, rules="R1x", drift=36).metrics["max_lead"] <= 36
    assert run(sc, rules="R1x", drift=120).metrics["max_lead"] > 72


def test_free_wait_window_moves_where_easing_starts():
    parent = (BASE_TIME + 6 * 50, 145050)

    def target(gap, free):
        return bits_to_target(asert.next_bits(*parent, BASE_TIME, 145000, 0x1F7FFFFF, 600,
                                              candidate_time=parent[0] + gap, tau_wait_s=60, free_wait_s=free))

    assert target(70, free=72) == target(1, free=72)       # still inside the free window
    assert target(72 + 60, free=72) == pytest.approx(2 * target(70, free=72), rel=1e-4)
    assert target(72 + 60, free=36) > target(72 + 60, free=72)  # a shorter window eases sooner


def test_r6_drift_and_free_wait_are_parameters():
    assert make_rules("R6", 4, drift=120).drift == 120
    assert make_rules("R6d", 4, free_wait_s=72).asert_free == 72
    for name in ("R1a", "R2a", "R5a"):
        assert make_rules(name, 4, free_wait_s=72).asert_free == 72


def test_expand_jobs_is_the_cross_product_of_configs_scenarios_and_seeds():
    spec = {"seeds": 2,
            "scenarios": [{"label": "a", "file": "far_future.json", "metric": "max_lead"},
                          {"label": "b", "file": "far_future.json", "metric": "max_lead"}],
            "configs": [{"label": "c1", "rule": "R1", "params": {}}, {"label": "c2", "rule": "R6d", "params": {}}]}
    jobs = sweep.expand_jobs(spec)
    assert len(jobs) == 2 * 2 * 2
    assert {(j["config"], j["scenario"], j["seed"]) for j in jobs} >= {("c1", "a", 1), ("c2", "b", 2)}


def test_override_sets_a_miner_field_and_a_top_level_field():
    sc = {"duration_s": 100, "miners": [{"name": "burst", "hashrate": 100}, {"name": "honest", "hashrate": 1}]}
    out = sweep.apply_override(sc, {"burst.hashrate": 10, "duration_s": 50})
    assert out["miners"][0]["hashrate"] == 10 and out["miners"][1]["hashrate"] == 1 and out["duration_s"] == 50
    assert sc["miners"][0]["hashrate"] == 100  # the original is untouched


def test_a_scenario_can_show_several_metrics_as_separate_columns():
    spec = {"scenarios": [{"label": "burst", "file": "x.json", "metric": "post_burst_gap"},
                          {"label": "surge", "file": "y.json",
                           "columns": [{"label": "surge gap", "metric": "max_gap"},
                                       {"label": "surge stall", "metric": "total_stall", "worst": "max"}]}]}
    cols = sweep.columns_of(spec)
    assert [c["label"] for c in cols] == ["burst", "surge gap", "surge stall"]
    assert [c["scenario"] for c in cols] == ["burst", "surge", "surge"]


def test_write_rows_keeps_runs_whose_metrics_differ(tmp_path):
    # Surge runs have window metrics the others lack; one odd row must not lose the whole sweep.
    rows = [{"config": "a", "scenario": "x", "seed": 1, "max_gap": 5},
            {"config": "a", "scenario": "y", "seed": 1, "max_gap": 7, "window0_bph": 600.0}]
    sweep.write_rows(rows, str(tmp_path / "out"))
    import csv
    import json
    assert json.loads((tmp_path / "out.json").read_text()) == rows
    read = list(csv.DictReader(open(tmp_path / "out.csv")))
    assert [r["max_gap"] for r in read] == ["5", "7"]
    assert read[0]["window0_bph"] == "" and read[1]["window0_bph"] == "600.0"
