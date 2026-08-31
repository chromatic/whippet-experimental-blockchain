package main

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

// Differential tests for the incrementally-maintained lineage aggregates.
//
// /tokens used to recompute supply and holder counts by scanning every
// position on every request -- 474ms at 200k blocks, holding RLock, which
// stalls block ingestion as well as the request. The aggregates are now
// maintained as positions are created, spent, unspent and removed.
//
// Incremental maintenance is exactly the kind of code that drifts from the
// thing it replaced: it is correct on the paths a hand-written test
// happens to cover and wrong on the ones it does not. So rather than
// assert specific numbers, these tests assert that the incremental answer
// equals a full scan of the same state, after randomised sequences of
// applies and undoes. scanTokens below is the old implementation, kept as
// the oracle.

// scanTokens is the pre-aggregate implementation of AllTokens: read every
// position, group the unspent ones by origin, and total them in Go.
// Retained only as a test oracle. It deliberately does not consult the
// lineages table at all, so it can disagree with the maintained aggregate.
func scanTokens(t testing.TB, idx *Index) []TokenInfo {
	t.Helper()
	positions := allPositions(t, idx)
	// The mint of each lineage, spent or not: metadata belongs to the
	// lineage, and the mint is normally spent onward immediately.
	mintByOrigin := make(map[string]Position, len(positions))
	byOrigin := make(map[string][]Position)
	for _, pos := range positions {
		if pos.IsMint && pos.Origin != "" {
			mintByOrigin[pos.Origin] = pos
		}
		if pos.Origin == "" || pos.Spent {
			continue
		}
		byOrigin[pos.Origin] = append(byOrigin[pos.Origin], pos)
	}

	var out []TokenInfo
	for origin, group := range byOrigin {
		if len(group) == 0 {
			continue
		}
		var mintHeight int64
		multiplier := group[0].Multiplier
		for _, pos := range group {
			if pos.IsMint {
				mintHeight = pos.Height
				multiplier = pos.Multiplier
				break
			}
		}
		supply, holders := lineageSupplyAndHolders(group, multiplier)
		token := TokenInfo{
			Origin:     origin,
			Multiplier: multiplier,
			Supply:     supply,
			Holders:    holders,
			MintHeight: mintHeight,
		}
		// The lineage's declared ticker comes from the mint's own row,
		// found by (origin, is_mint) -- NOT by treating the origin as a
		// position key. That worked only while a v1 origin literally was
		// the mint's outpoint key; a v2 origin is SHA256 of it. Only the
		// mint is consulted, so a later holder cannot attach metadata of
		// their own.
		if mint, ok := mintByOrigin[origin]; ok && mint.Metadata != nil {
			token.Ticker = mint.Metadata.Ticker
			token.MetadataHash = mint.Metadata.MetadataHash
		}
		out = append(out, token)
	}
	return out
}

// lineageSupplyAndHolders totals the virtual balance of a lineage's
// positions and counts its distinct recipients, saturating rather than
// overflowing (see satMul's note on why a hostile mint makes that
// necessary). Part of the oracle, not the implementation.
func lineageSupplyAndHolders(positions []Position, multiplier int64) (int64, int) {
	var supply int64
	holders := make(map[string]bool)
	for _, pos := range positions {
		holders[pos.PubKey] = true
		supply = satAdd(supply, satMul(pos.Value, multiplier))
	}
	return supply, len(holders)
}

func sortTokens(ts []TokenInfo) {
	sort.Slice(ts, func(i, j int) bool { return ts[i].Origin < ts[j].Origin })
}

func assertTokensMatchScan(t *testing.T, idx *Index, context string) {
	t.Helper()
	got := mustAllTokens(t, idx)
	want := scanTokens(t, idx)
	sortTokens(got)
	sortTokens(want)

	if len(got) != len(want) {
		t.Fatalf("%s: AllTokens returned %d lineages, full scan found %d\n got: %+v\nwant: %+v",
			context, len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s: lineage %s differs\n incremental: %+v\n full scan:   %+v",
				context, want[i].Origin, got[i], want[i])
		}
	}

	// Token() must agree with AllTokens() entry for entry, since the API
	// serves both and a client comparing them would otherwise see the
	// same lineage report two different supplies.
	for _, w := range want {
		single, ok := mustToken(t, idx, w.Origin)
		if !ok {
			t.Errorf("%s: Token(%s) reports unknown, but it is in AllTokens", context, w.Origin)
			continue
		}
		if single != w {
			t.Errorf("%s: Token(%s) disagrees with the full scan\n got: %+v\nwant: %+v",
				context, w.Origin, single, w)
		}
	}
}

// livePos is an unspent position the generator can choose to spend, along
// with the multiplier a transfer of it must carry.
type livePos struct {
	txid string
	vout uint32
	mult int64
}

// randomChainBlock builds a block that mints, transfers, and re-spends
// earlier positions in unpredictable combinations.
//
// Transfers inherit their parent's multiplier, because consensus requires
// it (CheckUapOutputConservation). Generating mismatched multipliers
// within a lineage would test a state the chain cannot reach, and would
// do so against an oracle that is itself undefined there: the old scan
// picked positions[0].Multiplier, and positions[0] comes from Go map
// iteration order.
func randomChainBlock(t *testing.T, rng *rand.Rand, height int64, live *[]livePos) *RPCBlock {
	t.Helper()
	var txs []RPCTx

	nMints := rng.Intn(3)
	for m := 0; m < nMints; m++ {
		// Generate a valid 64-char hex txid
		txidBytes := make([]byte, 32)
		for j := 0; j < 32; j++ {
			txidBytes[j] = byte(rng.Intn(256))
		}
		txid := fmt.Sprintf("%064x", txidBytes)
		mult := int64(rng.Intn(2000))
		if rng.Intn(10) == 0 {
			mult = 0 // exercise the divide-by-zero guard
		}
		txs = append(txs, RPCTx{
			TxID: txid,
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{
				uapMintVout(0, fakePubKey(byte(0x02+rng.Intn(4))), mult, float64(rng.Intn(5)+1)),
				opReturnVout(1, wuapPayload("TK", make([]byte, 32))),
			},
		})
		*live = append(*live, livePos{txid: txid, vout: 0, mult: mult})
	}

	// Spend some live positions into transfers.
	nSpends := rng.Intn(3)
	for s := 0; s < nSpends && len(*live) > 0; s++ {
		i := rng.Intn(len(*live))
		parent := (*live)[i]
		*live = append((*live)[:i], (*live)[i+1:]...)

		// Generate a valid 64-char hex txid
		outTxIDBytes := make([]byte, 32)
		for j := 0; j < 32; j++ {
			outTxIDBytes[j] = byte(rng.Intn(256))
		}
		outTxID := fmt.Sprintf("%064x", outTxIDBytes)
		nOuts := 1 + rng.Intn(2)
		var vouts []RPCVout
		for o := 0; o < nOuts; o++ {
			vouts = append(vouts, uapTransferVout(uint32(o),
				fakePubKey(byte(0x02+rng.Intn(4))), parent.mult, originOfMint(t, parent.txid, parent.vout), float64(rng.Intn(3)+1)))
			*live = append(*live, livePos{txid: outTxID, vout: uint32(o), mult: parent.mult})
		}
		txs = append(txs, RPCTx{
			TxID: outTxID,
			Vin:  []RPCVin{spendVin(parent.txid, parent.vout)},
			Vout: vouts,
		})
	}

	if len(txs) == 0 {
		txs = append(txs, RPCTx{
			TxID: fmt.Sprintf("empty%d", height),
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{ordinaryVout(0, 1.0)},
		})
	}
	return makeBlock(fmt.Sprintf("h%d", height), height, txs)
}

// TestLineageAggregatesMatchFullScan drives randomised apply/undo
// sequences and checks the incremental aggregates against a full scan
// after every step.
func TestLineageAggregatesMatchFullScan(t *testing.T) {
	for seed := int64(0); seed < 25; seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			idx := NewIndex()
			var live []livePos

			// The set of spendable positions has to roll back with the
			// chain. Without this the generator kept handing out parents
			// that an undo had removed, and a re-applied block at the
			// same height minted a *different* multiplier into the same
			// origin key -- producing a lineage with mixed multipliers,
			// which the chain cannot actually contain.
			//
			// That state also makes the oracle nondeterministic: with no
			// unspent mint, scanTokens reports positions[0].Multiplier,
			// and positions[0] is whatever Go's map iteration yields
			// first. The old scan disagreed with itself on consecutive
			// calls. Keeping the generator honest keeps the comparison
			// meaningful.
			type appliedBlock struct {
				block *RPCBlock
				live  []livePos // snapshot from before it was applied
			}
			var applied []appliedBlock

			for step := 0; step < 40; step++ {
				// Undo sometimes, so spent positions come back and
				// created ones disappear -- the paths most likely to
				// leave an aggregate stale.
				if len(applied) > 0 && rng.Intn(3) == 0 {
					top := applied[len(applied)-1]
					applied = applied[:len(applied)-1]
					idx.UndoBlock(top.block.Height)
					live = top.live
					assertTokensMatchScan(t, idx, fmt.Sprintf("after undo at step %d", step))
					continue
				}
				height := int64(len(applied))
				snapshot := append([]livePos(nil), live...)
				b := randomChainBlock(t, rng, height, &live)
				idx.ApplyBlock(b)
				applied = append(applied, appliedBlock{block: b, live: snapshot})
				assertTokensMatchScan(t, idx, fmt.Sprintf("after apply at step %d", step))
			}

			// Unwind completely: every aggregate must drain to nothing.
			for i := len(applied) - 1; i >= 0; i-- {
				idx.UndoBlock(applied[i].block.Height)
			}
			assertTokensMatchScan(t, idx, "after full unwind")
			if got := mustAllTokens(t, idx); len(got) != 0 {
				t.Errorf("after unwinding every block, AllTokens still reports %d lineages: %+v", len(got), got)
			}
		})
	}
}

// TestLineageAggregatesSurviveSaveLoad verifies the aggregates are correct
// after a restart. They are derived state, so whether they are persisted
// or rebuilt on load is an implementation choice -- but getting it wrong
// yields an indexer that serves stale supply figures until the next
// restart, with nothing to indicate it.
func TestLineageAggregatesSurviveSaveLoad(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	path := t.TempDir() + "/state.sqlite"
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := store.LoadIndex()
	if err != nil {
		t.Fatal(err)
	}
	var live []livePos
	for h := 0; h < 30; h++ {
		idx.ApplyBlock(randomChainBlock(t, rng, int64(h), &live))
	}
	if err := idx.StoreErr(); err != nil {
		t.Fatal(err)
	}

	// A second handle onto the same file, as a restart would see it: only
	// data that actually committed. The first stays open so the two can
	// be compared.
	store2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	loaded, err := store2.LoadIndex()
	if err != nil {
		t.Fatal(err)
	}

	assertTokensMatchScan(t, loaded, "after save/load")

	before := mustAllTokens(t, idx)
	after := mustAllTokens(t, loaded)
	sortTokens(before)
	sortTokens(after)
	if len(before) != len(after) {
		t.Fatalf("lineage count changed across save/load: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("lineage %s changed across save/load:\nbefore: %+v\n after: %+v",
				before[i].Origin, before[i], after[i])
		}
	}
}
