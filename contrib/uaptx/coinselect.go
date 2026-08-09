package uaptx

import "fmt"

// COIN is the number of satoshis in one WHIP, matching src/amount.h.
const COIN = 100000000

// Policy constants from src/policy/policy.h and src/amount.h.
const (
	RecommendedMinTxFee  = COIN / 100 // 1000000 satoshis
	DefaultDustLimit     = RecommendedMinTxFee
	DefaultHardDustLimit = DefaultDustLimit / 10 // 100000 satoshis
)

// DefaultTransactionMaxFee mirrors src/validation.h's
// DEFAULT_TRANSACTION_MAXFEE: the node refuses to send a transaction paying
// more than this, because a fee rate is easy to get wrong by orders of
// magnitude and only the resulting absolute fee gives any clue.
const DefaultTransactionMaxFee = RecommendedMinTxFee * 10000 // 100 coins

// CandidateInput is a spendable UTXO offered to coin selection.
type CandidateInput struct {
	Txid       string
	Vout       uint32
	Value      int64
	ScriptCode []byte
	PrivKey    []byte
	PubKey     []byte
}

// estimateTxSize estimates transaction size in bytes for fee calculation.
// P2PKH inputs are ~150 bytes, outputs are ~34 bytes -- matching
// uap.js's estimateTxSize exactly (same magic constants), since this is a
// fee estimate that must produce the same transactions, not a precise
// byte count.
func estimateTxSize(nInputs, nOutputs int) int {
	// Base: version(4) + input count varint + output count varint + locktime(4)
	size := 10
	size += nInputs * 150
	size += nOutputs * 34
	return size
}

// SelectedCoins is the result of selectCoins.
type SelectedCoins struct {
	Selected   []CandidateInput
	TotalInput int64
	Change     int64
	Fee        int64
}

// selectCoins selects UTXOs from candidates to cover a target amount plus
// fee. Uses a greedy approach: sorts inputs by value ascending and selects
// the smallest UTXOs that sum to target + estimated fee. This selection
// strategy favors smaller inputs (maximizing input count), which
// consolidates dust but increases fees relative to selecting fewer large
// inputs -- matching uap.js's selectCoins exactly.
//
// feeRate is satoshis per kilobyte; changeScriptSize is the size in bytes
// of the change script, used only for the output-count/fee estimate.
func selectCoins(candidateInputs []CandidateInput, targetAmount int64, changeScriptSize int, feeRate int64, nOutputs int) (SelectedCoins, error) {
	sorted := make([]CandidateInput, len(candidateInputs))
	copy(sorted, candidateInputs)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j-1].Value > sorted[j].Value; j-- {
			sorted[j-1], sorted[j] = sorted[j], sorted[j-1]
		}
	}

	var selected []CandidateInput
	var totalInput int64

	for _, input := range sorted {
		selected = append(selected, input)
		totalInput += input.Value

		estimatedOutputs := nOutputs
		if totalInput > targetAmount && changeScriptSize > 0 {
			estimatedOutputs++
		}
		estimatedSize := estimateTxSize(len(selected), estimatedOutputs)
		estimatedFee := (int64(estimatedSize)*feeRate + 999) / 1000 // ceil(size * feeRate / 1000)

		if estimatedFee < RecommendedMinTxFee {
			estimatedFee = RecommendedMinTxFee
		}

		if totalInput >= targetAmount+estimatedFee {
			change := totalInput - targetAmount - estimatedFee
			return SelectedCoins{Selected: selected, TotalInput: totalInput, Change: change, Fee: estimatedFee}, nil
		}
	}

	return SelectedCoins{}, fmt.Errorf("insufficient funds for transaction (cannot select enough UTXOs to cover target + fee)")
}
