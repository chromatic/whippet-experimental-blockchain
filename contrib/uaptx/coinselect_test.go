package uaptx

import "testing"

func TestEstimateTxSize(t *testing.T) {
	// 10 base + 150*inputs + 34*outputs, matching uap.js's estimateTxSize
	// magic constants exactly (this is a fee estimate that must produce
	// the same transactions as the JS wallet, not a precise byte count).
	got := estimateTxSize(2, 3)
	want := 10 + 2*150 + 3*34
	if got != want {
		t.Errorf("got %d want %d", got, want)
	}
}

func TestSelectCoinsPicksSmallestFirst(t *testing.T) {
	candidates := []CandidateInput{
		{Txid: "big", Value: 100 * COIN},
		{Txid: "small", Value: 2 * COIN},
		{Txid: "mid", Value: 5 * COIN},
	}
	result, err := selectCoins(candidates, 1*COIN, 25, 1000, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Selected) != 1 || result.Selected[0].Txid != "small" {
		t.Fatalf("expected the single smallest UTXO to suffice, got %+v", result.Selected)
	}
	if result.Fee != RecommendedMinTxFee {
		t.Errorf("expected fee to hit the RecommendedMinTxFee floor, got %d", result.Fee)
	}
	wantChange := result.TotalInput - 1*COIN - result.Fee
	if result.Change != wantChange {
		t.Errorf("change = %d, want %d", result.Change, wantChange)
	}
}

func TestSelectCoinsAccumulatesMultipleInputs(t *testing.T) {
	candidates := []CandidateInput{
		{Txid: "a", Value: 1 * COIN},
		{Txid: "b", Value: 1 * COIN},
		{Txid: "c", Value: 1 * COIN},
	}
	// Target requires more than one small UTXO, but leaves enough of the
	// third candidate that it should NOT be needed.
	target := int64(1.5 * COIN)
	result, err := selectCoins(candidates, target, 25, 1000, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Selected) != 2 {
		t.Fatalf("expected 2 inputs selected, got %d: %+v", len(result.Selected), result.Selected)
	}
	if result.TotalInput != 2*COIN {
		t.Errorf("totalInput = %d, want %d", result.TotalInput, 2*COIN)
	}
	if result.TotalInput < target+RecommendedMinTxFee {
		t.Errorf("totalInput %d does not cover target %d + fee floor", result.TotalInput, target)
	}
}

func TestSelectCoinsInsufficientFundsErrors(t *testing.T) {
	candidates := []CandidateInput{
		{Txid: "a", Value: 1000},
	}
	_, err := selectCoins(candidates, 1*COIN, 25, 1000, 1)
	if err == nil {
		t.Fatal("expected an insufficient-funds error")
	}
}

func TestSelectCoinsFeeRateAboveFloorIsRespected(t *testing.T) {
	// With a high feeRate the computed fee should exceed the
	// RecommendedMinTxFee floor, and selectCoins must account for it
	// (not just the floor) when deciding whether totalInput covers
	// target+fee.
	candidates := []CandidateInput{
		{Txid: "a", Value: 50 * COIN},
	}
	const highFeeRate = 10_000_000 // sat/kB, deliberately large
	result, err := selectCoins(candidates, 1*COIN, 25, highFeeRate, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	estimatedSize := estimateTxSize(1, 2) // 1 input + payment + change
	wantFee := int64((estimatedSize*highFeeRate + 999) / 1000)
	if result.Fee != wantFee {
		t.Errorf("fee = %d, want %d (estimatedSize=%d)", result.Fee, wantFee, estimatedSize)
	}
	if result.Fee <= RecommendedMinTxFee {
		t.Fatalf("test setup error: fee did not exceed the floor, adjust highFeeRate")
	}
}
