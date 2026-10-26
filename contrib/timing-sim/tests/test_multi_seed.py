"""Aggregation for the multi-seed runner (no Docker needed)."""
import multi_seed


def test_summarize_reports_median_and_worst_case():
    summary = multi_seed.summarize([5, 1, 9, 3, 7])
    assert summary == {"median": 5, "worst": 9}


def test_summarize_of_even_count_uses_the_lower_middle_value():
    assert multi_seed.summarize([4, 2, 8, 6])["median"] == 4
