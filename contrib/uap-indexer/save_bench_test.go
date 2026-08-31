package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// buildScaleIndex creates an index with n mint positions, n transfer
// positions and n UTXOs, roughly what n blocks of steady activity gives.
func buildScaleIndex(t testing.TB, n int) *Index {
	idx := NewIndex()
	for _, blk := range scaleBlocks(t, n) {
		idx.ApplyBlock(blk)
	}
	return idx
}

// scaleBlocks builds the same n blocks buildScaleIndex applies, so a
// benchmark can hand them to an index one at a time and time only that.
func scaleBlocks(t testing.TB, n int) []*RPCBlock {
	out := make([]*RPCBlock, 0, n)
	for i := 0; i < n; i++ {
		pk := fakePubKey(byte(0x02 + i%2))
		// Generate valid 64-char hex txids
		mintTxIDBytes := make([]byte, 32)
		for j := 0; j < 32; j++ {
			mintTxIDBytes[j] = byte((i*256 + j) & 0xff)
		}
		mintTxID := fmt.Sprintf("%064x", mintTxIDBytes)
		out = append(out, makeBlock(fmt.Sprintf("h%d", i), int64(i), []RPCTx{
			{
				TxID: mintTxID,
				Vin:  []RPCVin{coinbaseVin()},
				Vout: []RPCVout{
					uapMintVout(0, pk, int64(10+i), 1.0),
					opReturnVout(1, wuapPayload("TK", make([]byte, 32))),
					p2pkhVoutFor(2, byte(i), 2.0),
				},
			},
			func() RPCTx {
				origin, _ := UapOriginFromOutpoint(mintTxID, 0)
				return RPCTx{
					TxID: fmt.Sprintf("%064x", []byte{byte((i*256 + 1) & 0xff)}),
					Vin:  []RPCVin{spendVin(mintTxID, 0)},
					Vout: []RPCVout{uapTransferVout(0, fakePubKey(0x03), int64(10+i), origin, 1.0)},
				}
			}(),
		}))
	}
	return out
}

// BenchmarkStoreApplyBlock measures the per-block cost of persisting
// through the store at a range of index sizes. The figure that matters is
// that it does not move with n.
//
// It replaced a whole-state JSON snapshot, which measured 2,399ms and
// 184MB at 200k blocks -- the numbers that motivated the change. That
// benchmark is gone with the maps it marshalled; re-creating them here
// just to keep it runnable would mean maintaining a second copy of the
// index for the sake of a comparison already made.
func BenchmarkStoreApplyBlock(b *testing.B) {
	for _, n := range []int{1000, 10000, 50000, 200000} {
		b.Run(fmt.Sprintf("blocks=%d", n), func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "state.sqlite")
			store, err := OpenStore(path)
			if err != nil {
				b.Fatal(err)
			}
			defer store.Close()
			idx, err := store.LoadIndex()
			if err != nil {
				b.Fatal(err)
			}
			for _, blk := range scaleBlocks(b, n) {
				idx.ApplyBlock(blk)
			}
			if err := idx.StoreErr(); err != nil {
				b.Fatal(err)
			}
			extra := scaleBlocks(b, n+b.N)[n:]
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				idx.ApplyBlock(extra[i])
			}
			b.StopTimer()
			if err := idx.StoreErr(); err != nil {
				b.Fatal(err)
			}
			st, _ := os.Stat(path)
			b.ReportMetric(float64(st.Size())/1e6, "MB_state")
		})
	}
}

func BenchmarkStoreLoadIndex(b *testing.B) {
	for _, n := range []int{10000, 200000} {
		b.Run(fmt.Sprintf("blocks=%d", n), func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "state.sqlite")
			store, err := OpenStore(path)
			if err != nil {
				b.Fatal(err)
			}
			defer store.Close()
			idx, err := store.LoadIndex()
			if err != nil {
				b.Fatal(err)
			}
			for _, blk := range scaleBlocks(b, n) {
				idx.ApplyBlock(blk)
			}
			if err := idx.StoreErr(); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := store.LoadIndex(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkAllTokensInMemory measures the /tokens endpoint as it works
// it is now: a scan of the maintained lineages table, whose size is the
// number of tokens rather than the number of positions.
func BenchmarkAllTokensFromStore(b *testing.B) {
	for _, n := range []int{50000, 200000} {
		b.Run(fmt.Sprintf("blocks=%d", n), func(b *testing.B) {
			idx := buildScaleIndex(b, n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if got := len(mustAllTokens(b, idx)); got == 0 {
					b.Fatal("no tokens")
				}
			}
		})
	}
}

// buildRealisticIndex mints `lineages` tokens up front and then only
// transfers them, which is the shape a real market has: many positions
// spread across comparatively few tokens. buildScaleIndex mints a new
// token every block, so it has as many lineages as positions -- the worst
// possible case for a per-lineage aggregate, and not a realistic one.
func buildRealisticIndex(blocks, lineages int) *Index {
	idx := NewIndex()
	var txs []RPCTx
	for i := 0; i < lineages; i++ {
		txs = append(txs, RPCTx{
			TxID: txid(fmt.Sprintf("mint%d", i)),
			Vin:  []RPCVin{coinbaseVin()},
			Vout: []RPCVout{uapMintVout(0, fakePubKey(0x02), int64(10+i), 1.0)},
		})
	}
	idx.ApplyBlock(makeBlock("h0", 0, txs))

	// Each later block transfers one token onward to a new holder.
	prev := make([]string, lineages)
	for i := range prev {
		prev[i] = txid(fmt.Sprintf("mint%d", i))
	}
	for h := 1; h < blocks; h++ {
		i := h % lineages
		txid := fmt.Sprintf("x%d", h)
		origin, _ := UapOriginFromOutpoint(prev[i], 0)
		idx.ApplyBlock(makeBlock(fmt.Sprintf("h%d", h), int64(h), []RPCTx{{
			TxID: txid,
			Vin:  []RPCVin{spendVin(prev[i], 0)},
			Vout: []RPCVout{uapTransferVout(0, fakePubKey(byte(h%200)), int64(10+i), origin, 1.0)},
		}}))
		prev[i] = txid
	}
	return idx
}

func BenchmarkAllTokensRealistic(b *testing.B) {
	for _, lineages := range []int{50, 5000} {
		b.Run(fmt.Sprintf("blocks=200000/lineages=%d", lineages), func(b *testing.B) {
			idx := buildRealisticIndex(200000, lineages)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if len(mustAllTokens(b, idx)) != lineages {
					b.Fatal("wrong lineage count")
				}
			}
		})
	}
}

// BenchmarkAllTokensScanBaseline is the pre-aggregate implementation on
// the same fixture, so the before/after is measured rather than asserted.
func BenchmarkAllTokensScanBaseline(b *testing.B) {
	for _, lineages := range []int{50, 5000} {
		b.Run(fmt.Sprintf("blocks=200000/lineages=%d", lineages), func(b *testing.B) {
			idx := buildRealisticIndex(200000, lineages)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if len(scanTokens(b, idx)) != lineages {
					b.Fatal("wrong lineage count")
				}
			}
		})
	}
}
