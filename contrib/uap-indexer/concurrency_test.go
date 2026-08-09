package main

import (
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// Concurrency tests for the extended index state (UTXOs, lineage origins,
// mint metadata, orders).
//
// The indexer has exactly one writer -- the block-polling goroutine -- and
// many concurrent readers, one per in-flight HTTP request. Every read
// method is expected to hold idx.mu and to hand back *copies*, never
// pointers into the live maps, because the writer mutates those maps and
// the Position structs inside them (ApplyBlock flips pos.Spent in place).
//
// That "copy, don't alias" rule is easy to break silently: returning
// []*Position instead of []Position, or forgetting a lock on a new
// accessor, produces code that behaves perfectly in every serial test and
// races only under production load. These tests exist to make that break
// loud. They are only meaningful under `go test -race` -- without it they
// still exercise the code but the detector is what actually fails them.
// `make test` passes -race for this reason.
//
// Note the Metadata field in particular: Position is copied by value, but
// it holds a *TokenMetadata. A by-value copy still aliases that pointer,
// so the readers below deliberately dereference it. If the writer ever
// starts mutating metadata in place rather than replacing the pointer,
// that is a real race and this is what will catch it.

// chainBlock is one synthetic block plus the facts a serial replay needs
// to assert against.
type chainBlock struct {
	block *RPCBlock
}

func p2pkhVoutFor(n uint32, seed byte, value float64) RPCVout {
	return voutWithScript(n, value, p2pkhScriptBytes(seededHash160(seed)))
}

func hash160Hex(seed byte) string {
	return hex.EncodeToString(seededHash160(seed))
}

// buildChain produces a deterministic chain that touches every kind of
// state the index tracks: mints (with metadata), transfers that inherit a
// lineage origin, plain P2PKH UTXOs, and spends of both positions and
// UTXOs. Heights start at 0 so the whole chain can be applied and undone
// as a unit.
func buildChain(n int) []chainBlock {
	salt := []byte("0123456789abcdef")
	out := make([]chainBlock, 0, n)

	for i := 0; i < n; i++ {
		height := int64(i)
		mintTxID := fmt.Sprintf("mint%d", i)
		payTxID := fmt.Sprintf("pay%d", i)
		pubkey := fakePubKey(byte(0x02 + i%2))

		txs := []RPCTx{
			// A mint, its metadata declaration, and an ordinary payment
			// output, all in one transaction.
			{
				TxID: mintTxID,
				Vin:  []RPCVin{coinbaseVin()},
				Vout: []RPCVout{
					uapMintVout(0, pubkey, int64(10+i), salt, 1.0),
					opReturnVout(1, wuapPayload(
						fmt.Sprintf("TK%d", i),
						make([]byte, 32),
					)),
					p2pkhVoutFor(2, byte(i+1), 2.0),
				},
			},
			// A transfer spending that mint, so the position gets marked
			// spent and a child position inherits the lineage origin.
			{
				TxID: payTxID,
				Vin:  []RPCVin{spendVin(mintTxID, 0)},
				Vout: []RPCVout{
					uapTransferVout(0, fakePubKey(0x03), int64(10+i), 1.0),
				},
			},
		}

		// From the second block on, also spend the previous block's
		// P2PKH UTXO, exercising the UTXO undo path.
		if i > 0 {
			txs = append(txs, RPCTx{
				TxID: fmt.Sprintf("spend%d", i),
				Vin:  []RPCVin{spendVin(fmt.Sprintf("mint%d", i-1), 2)},
				Vout: []RPCVout{p2pkhVoutFor(0, byte(100+i), 1.0)},
			})
		}

		out = append(out, chainBlock{
			block: makeBlock(fmt.Sprintf("hash%d", i), height, txs),
		})
	}
	return out
}

// applyAll / undoAll drive the writer side.
func applyAll(idx *Index, chain []chainBlock) {
	for _, cb := range chain {
		idx.ApplyBlock(cb.block)
	}
}

func undoAll(idx *Index, chain []chainBlock) {
	undoFrom(idx, chain, 0)
}

// undoFrom rolls back chain[keep:], newest first, leaving chain[:keep]
// applied.
func undoFrom(idx *Index, chain []chainBlock, keep int) {
	for i := len(chain) - 1; i >= keep; i-- {
		idx.UndoBlock(chain[i].block.Height)
	}
}

// TestConcurrentReadsDuringApplyAndUndo hammers every read accessor while a
// writer goroutine repeatedly applies and rolls back the whole chain.
//
// Under -race this fails if any accessor reads shared state without the
// lock, or hands back a pointer the writer can still mutate.
func TestConcurrentReadsDuringApplyAndUndo(t *testing.T) {
	const chainLen = 12
	const settleAt = chainLen / 2
	const rounds = 30
	const readers = 8

	// Attached to a store so every write path also goes through SQLite
	// under the same lock, and the race detector sees it. This is where
	// the old test called Save from a reader goroutine, which was the
	// right check for a snapshot writer that marshalled the live maps;
	// with per-block writes there is nothing left for a reader to do.
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	idx, err := store.LoadIndex()
	if err != nil {
		t.Fatalf("LoadIndex: %v", err)
	}
	chain := buildChain(chainLen)

	// Prime the index once so readers have something to look at from the
	// very first iteration, and so PublishOrder below has a live position.
	applyAll(idx, chain)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writer: churn the chain forwards and backwards. This is the only
	// goroutine driving blocks, matching production, where a single poller
	// owns chain advancement.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for r := 0; r < rounds; r++ {
			undoAll(idx, chain)
			applyAll(idx, chain)
		}
		// Settle half-rolled-back, NOT fully applied. This matters: each
		// block from the second on spends the previous block's P2PKH
		// output, so ending on a full apply would re-spend everything
		// undo had just restored and the two states would agree even if
		// UndoBlock restored nothing at all. Stopping mid-chain leaves
		// restored-UTXO state visible in the final comparison, which is
		// the only way the assertion can see that undo did its job.
		undoFrom(idx, chain, settleAt)
	}()

	// Readers: call everything the HTTP API can call.
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			pubkeyHex := hex.EncodeToString(fakePubKey(0x02))
			transferPubKeyHex := hex.EncodeToString(fakePubKey(0x03))
			for {
				select {
				case <-stop:
					return
				default:
				}

				for _, pk := range []string{pubkeyHex, transferPubKeyHex} {
					for _, unspentOnly := range []bool{true, false} {
						positions, _ := idx.PositionsForPubKey(pk, unspentOnly)
						for _, pos := range positions {
							// Touch every field, including through the
							// Metadata pointer that a by-value copy still
							// aliases.
							_ = pos.TxID + pos.PubKey + pos.Origin + pos.SpentTxID
							_ = pos.Multiplier + pos.Value + pos.Height + pos.SpentHeight
							if pos.Metadata != nil {
								_ = pos.Metadata.Ticker + pos.Metadata.MetadataHash
							}
						}
					}
				}

				for i := 0; i < chainLen; i++ {
					_, _, _ = idx.Position(fmt.Sprintf("mint%d", i), 0)
					_, _, _ = idx.UTXO(fmt.Sprintf("mint%d", i), 2)
					idx.HashAtHeight(int64(i))
					utxos, _ := idx.UTXOsForHash160(hash160Hex(byte(i + 1)))
					for _, u := range utxos {
						_ = u.TxID + u.Hash160
						_ = u.Value + u.Height
					}
				}

				// Errors are ignored throughout this goroutine: t.Fatalf
				// is not legal off the test goroutine, and what is being
				// tested is that these run concurrently with the writer
				// without tripping the race detector or corrupting a
				// result -- not that each individual call succeeds.
				tokens, _ := idx.AllTokens()
				for _, tok := range tokens {
					_ = tok.Ticker + tok.MetadataHash + tok.Origin
					_, _, _ = idx.Token(tok.Origin)
				}

				_, _ = idx.StatusSnapshot()
				_, _ = idx.ListOrders(nil)
				m := int64(10)
				_, _ = idx.ListOrders(&m)
				_, _, _ = idx.GetOrder("mint0", 0)
			}
		}(i)
	}

	// Order churn: publish and cancel against positions that the writer is
	// concurrently creating and destroying. Both outcomes are legitimate
	// here -- the position may or may not exist at any instant -- so
	// errors are expected and ignored. What is being tested is that the
	// mutation is properly serialised, not that it succeeds.
	wg.Add(1)
	go func() {
		defer wg.Done()
		scriptSig := makeOrderScriptSig()
		for {
			select {
			case <-stop:
				return
			default:
			}
			for i := 0; i < chainLen; i++ {
				o := &Order{
					TxID:          fmt.Sprintf("mint%d", i),
					Vout:          0,
					Multiplier:    int64(10 + i),
					ScriptSig:     scriptSig,
					PaymentScript: "76a914" + hash160Hex(0x01) + "88ac",
					PaymentValue:  5000000,
				}
				_ = idx.PublishOrder(o)
				_ = idx.CancelOrder(o.TxID, o.Vout, scriptSig)
			}
		}
	}()

	wg.Wait()

	// The index must now match a clean serial replay of the blocks that
	// are still supposed to be applied. Without this the test would only
	// prove "no race detected", not that the churn left the state
	// correct -- an undo path that dropped entries would still pass.
	want := NewIndex()
	applyAll(want, chain[:settleAt])

	got := mustStatus(t, idx)
	expect := mustStatus(t, want)
	if got.TipHeight != expect.TipHeight || got.TipHash != expect.TipHash {
		t.Errorf("tip after churn: got %d/%s, want %d/%s",
			got.TipHeight, got.TipHash, expect.TipHeight, expect.TipHash)
	}
	if got.PositionCount != expect.PositionCount {
		t.Errorf("position count after churn: got %d, want %d",
			got.PositionCount, expect.PositionCount)
	}

	assertSamePositions(t, idx, want)
	assertSameUTXOs(t, idx, want)
}

// makeOrderScriptSig returns a structurally valid scriptSig: a single
// minimal push of a DER-shaped blob ending in SIGHASH_SINGLE|ANYONECANPAY.
// It is not a real signature -- PublishOrder only checks shape (see
// validateScriptSig's own note on why).
func makeOrderScriptSig() string {
	der := []byte{0x30, 0x06, 0x02, 0x01, 0x01, 0x02, 0x01, 0x01}
	sig := append(der, sighashOrderType)
	out := append([]byte{byte(len(sig))}, sig...)
	return hex.EncodeToString(out)
}

func assertSamePositions(t *testing.T, got, want *Index) {
	t.Helper()
	assertSameSet(t, "position", "after churn",
		allPositions(t, got), allPositions(t, want),
		func(pos Position) string { return positionKey(pos.TxID, pos.Vout) },
		// Position is compared field by field rather than with ==: its
		// Metadata is a pointer, so == would ask whether the two indexes
		// happen to share an allocation instead of whether the metadata
		// itself matches.
		func(t testing.TB, k string, gp, wp Position) {
			t.Helper()
			if gp.Spent != wp.Spent || gp.Value != wp.Value || gp.Multiplier != wp.Multiplier ||
				gp.Origin != wp.Origin || gp.PubKey != wp.PubKey || gp.Height != wp.Height ||
				gp.SpentTxID != wp.SpentTxID || gp.SpentHeight != wp.SpentHeight {
				t.Errorf("position %s differs after churn:\n got %+v\nwant %+v", k, gp, wp)
			}
			switch {
			case (gp.Metadata == nil) != (wp.Metadata == nil):
				t.Errorf("position %s metadata presence differs: got %v want %v",
					k, gp.Metadata, wp.Metadata)
			case gp.Metadata != nil && *gp.Metadata != *wp.Metadata:
				t.Errorf("position %s metadata differs: got %+v want %+v",
					k, *gp.Metadata, *wp.Metadata)
			}
		})
}

func assertSameUTXOs(t *testing.T, got, want *Index) {
	t.Helper()
	assertSameSet(t, "utxo", "after churn",
		allUTXOs(t, got), allUTXOs(t, want),
		func(u UTXO) string { return positionKey(u.TxID, u.Vout) },
		diffComparable[UTXO]("utxo", "after churn"))
}

// TestConcurrentReorgWithExtendedState drives competing chains rather than
// straight apply/undo: the writer rolls back to a fork point and applies a
// different continuation, while readers keep querying. This is the shape a
// real reorg takes, and it exercises undo of UTXOs, lineage origins and
// metadata together rather than one at a time.
func TestConcurrentReorgWithExtendedState(t *testing.T) {
	const rounds = 40
	const readers = 6

	idx := NewIndex()
	common := buildChain(4)
	applyAll(idx, common)

	// Two continuations from height 4, differing in every indexed field.
	branchA := forkAt(4, "A")
	branchB := forkAt(4, "B")

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for r := 0; r < rounds; r++ {
			branch := branchA
			if r%2 == 1 {
				branch = branchB
			}
			applyAll(idx, branch)
			undoAll(idx, branch)
		}
		applyAll(idx, branchA)
	}()

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// Errors ignored: t.Fatalf is not legal off the test
				// goroutine, and this loop exists to run reads against a
				// concurrent writer, not to assert on each one.
				tokens, _ := idx.AllTokens()
				for _, tok := range tokens {
					_ = tok.Ticker
					_, _, _ = idx.Token(tok.Origin)
				}
				_, _ = idx.PositionsForPubKey(hex.EncodeToString(fakePubKey(0x02)), false)
				_, _ = idx.PositionsForPubKey(hex.EncodeToString(fakePubKey(0x03)), true)
				_, _ = idx.UTXOsForHash160(hash160Hex(0x01))
				_, _ = idx.StatusSnapshot()
				idx.HashAtHeight(4)
			}
		}()
	}

	wg.Wait()

	// After settling on branch A, the index must equal a serial replay of
	// common + branchA -- no residue from the discarded branch B.
	want := NewIndex()
	applyAll(want, common)
	applyAll(want, branchA)

	if got, expect := mustStatus(t, idx), mustStatus(t, want); got != expect {
		t.Errorf("status after reorg churn: got %+v, want %+v", got, expect)
	}
	assertSamePositions(t, idx, want)
	assertSameUTXOs(t, idx, want)

	// The discarded branch must leave nothing behind. Checking the tip
	// alone would miss orphaned positions whose block was undone but whose
	// entries were not.
	if _, ok := mustPosition(t, idx, "mintB0", 0); ok {
		t.Error("branch B position survived a reorg onto branch A")
	}
	if _, ok := mustUTXO(t, idx, "mintB0", 2); ok {
		t.Error("branch B UTXO survived a reorg onto branch A")
	}
	if _, ok := mustUTXO(t, idx, "mint3", 2); !ok {
		t.Error("common-chain UTXO spent by branch B was not restored when branch B was rolled back")
	}
}

// forkAt builds a two-block continuation starting at the given height,
// tagged so the two branches share no txids, hashes or scripts.
func forkAt(height int64, tag string) []chainBlock {
	salt := []byte("0123456789abcdef")
	seed := byte(0x10)
	if tag == "B" {
		seed = byte(0x40)
	}

	var out []chainBlock
	for i := 0; i < 2; i++ {
		h := height + int64(i)
		mintTxID := fmt.Sprintf("mint%s%d", tag, i)
		txs := []RPCTx{
			{
				TxID: mintTxID,
				Vin:  []RPCVin{coinbaseVin()},
				Vout: []RPCVout{
					uapMintVout(0, fakePubKey(0x02), int64(500+i), salt, 3.0),
					opReturnVout(1, wuapPayload(tag+"TK", make([]byte, 32))),
					p2pkhVoutFor(2, seed+byte(i), 4.0),
				},
			},
			{
				TxID: fmt.Sprintf("xfer%s%d", tag, i),
				Vin:  []RPCVin{spendVin(mintTxID, 0)},
				Vout: []RPCVout{uapTransferVout(0, fakePubKey(0x03), int64(500+i), 3.0)},
			},
		}

		// Branch B -- and only branch B -- spends a P2PKH UTXO from the
		// common chain. That asymmetry is deliberate: the test settles on
		// branch A, so mint3:2 is unspent in the expected state and can
		// only be unspent in the actual state if undoing branch B put it
		// back. If both branches spent it, or neither did, the final
		// comparison would agree whether or not UndoBlock restored
		// anything, and the assertion would be decorative.
		if tag == "B" && i == 0 {
			txs = append(txs, RPCTx{
				TxID: "spendCommonB",
				Vin:  []RPCVin{spendVin("mint3", 2)},
				Vout: []RPCVout{p2pkhVoutFor(0, 0x70, 1.0)},
			})
		}
		out = append(out, chainBlock{
			block: makeBlock(fmt.Sprintf("hash%s%d", tag, i), h, txs),
		})
	}
	return out
}
