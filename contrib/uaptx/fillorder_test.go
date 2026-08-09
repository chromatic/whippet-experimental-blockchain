package uaptx

// TestFillOrderMatchesJSFixture cross-checks FillOrder + SerializeTx
// against a full transaction assembled by uap-js's signMakerOrder +
// fillOrder + txToHex. A sighash-level match does not prove the *assembled*
// transaction (vin/vout ordering, scriptSig placement, change handling)
// matches -- this does.
//
// Fixture generated (safe: pure local computation, no network):
//
//	cd contrib/uap-js
//	node -e '
//	  import("./secp.js").then(async (secp) => {
//	    const UAP = await import("./uap.js");
//	    UAP.configureSecp(secp);
//	    const makerPriv = UAP.hexToBytes("01".repeat(32));
//	    const makerPub = secp.getPublicKey(makerPriv, true);
//	    const scriptCode = UAP.buildTransferScript(makerPub, 7);
//	    const order = UAP.signMakerOrder(secp, {
//	      input: { txid: "aa".repeat(32), vout: 0, scriptCode },
//	      privKey: makerPriv,
//	      paymentScript: UAP.hexToBytes("76a914" + "11".repeat(20) + "88ac"),
//	      paymentValue: 5 * UAP.COIN,
//	    });
//	    const tx = UAP.fillOrder(order, {
//	      toPubkey: UAP.hexToBytes("02".repeat(33)),
//	      multiplier: 7,
//	      tokenValue: 123456789,
//	      takerInputs: [
//	        { txid: "bb".repeat(32), vout: 1 },
//	        { txid: "cc".repeat(32), vout: 3 },
//	      ],
//	      changeScript: UAP.hexToBytes("76a91488888888888888888888888888888888888888ff88ac"),
//	      changeValue: 999999,
//	    });
//	    console.log(UAP.txToHex(tx));
//	  });'

import (
	"encoding/hex"
	"testing"
)

func TestFillOrderMatchesJSFixture(t *testing.T) {
	order := MakerOrder{
		ScriptSig:     mustHex(t, "483045022100f1aa3a84b72f00b9cc1dc7e1d363eff8f6a5491366f92d6b2300d18087e7a625022006703dd7d86b29d1067c445e3eb6a76549800f1467d5c8a2176a0a3dcbe50de883"),
		Txid:          "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Vout:          0,
		PaymentScript: mustHex(t, "76a914111111111111111111111111111111111111111188ac"),
		PaymentValue:  500000000,
	}
	takerPub := mustHex(t, "020202020202020202020202020202020202020202020202020202020202020202")
	changeScript := mustHex(t, "76a91488888888888888888888888888888888888888ff88ac")

	tx := FillOrder(order, FillOrderOpts{
		ToPubkey:   takerPub,
		Multiplier: 7,
		TokenValue: 123456789,
		TakerInputs: []TxIn{
			{Txid: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Vout: 1},
			{Txid: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", Vout: 3},
		},
		ChangeScript: changeScript,
		ChangeValue:  999999,
	})

	got := TxToHex(tx)
	want := "0100000003aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa0000000049483045022100f1aa3a84b72f00b9cc1dc7e1d363eff8f6a5491366f92d6b2300d18087e7a625022006703dd7d86b29d1067c445e3eb6a76549800f1467d5c8a2176a0a3dcbe50de883ffffffffbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb0100000000ffffffffcccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc0300000000ffffffff030065cd1d000000001976a914111111111111111111111111111111111111111188ac15cd5b0700000000242102020202020202020202020202020202020202020202020202020202020202020257ba3f420f00000000001976a91488888888888888888888888888888888888888ff88ac00000000"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestFillOrderNoChangeOutputWhenChangeValueZero(t *testing.T) {
	order := MakerOrder{
		ScriptSig:     []byte{0x01},
		Txid:          "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Vout:          0,
		PaymentScript: []byte{0x51},
		PaymentValue:  1000,
	}
	tx := FillOrder(order, FillOrderOpts{
		ToPubkey:    []byte{0x02},
		Multiplier:  1,
		TokenValue:  2000,
		TakerInputs: nil,
		ChangeValue: 0, // no change
	})
	if len(tx.Vout) != 2 {
		t.Fatalf("expected exactly 2 outputs (payment + token) with no change, got %d: %+v", len(tx.Vout), tx.Vout)
	}
}

func TestFillOrderOmitsPaymentOutputWhenPaymentScriptNil(t *testing.T) {
	// Matches JS's `order.paymentScript !== undefined` check: a maker order
	// with no payment output at all must not synthesize one.
	order := MakerOrder{
		ScriptSig:     []byte{0x01},
		Txid:          "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Vout:          0,
		PaymentScript: nil,
	}
	tx := FillOrder(order, FillOrderOpts{
		ToPubkey:   []byte{0x02},
		Multiplier: 1,
		TokenValue: 2000,
	})
	if len(tx.Vout) != 1 {
		t.Fatalf("expected exactly 1 output (token only), got %d: %+v", len(tx.Vout), tx.Vout)
	}
	if tx.Vout[0].Value != 2000 {
		t.Errorf("expected the token output to be vout[0], got %+v", tx.Vout[0])
	}
}

func TestFillOrderKeepsMakerInputAtIndexZero(t *testing.T) {
	order := MakerOrder{
		ScriptSig:     []byte{0x01},
		Txid:          "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Vout:          0,
		PaymentScript: []byte{0x51},
		PaymentValue:  1000,
	}
	tx := FillOrder(order, FillOrderOpts{
		ToPubkey:   []byte{0x02},
		Multiplier: 1,
		TokenValue: 2000,
		TakerInputs: []TxIn{
			{Txid: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Vout: 1},
		},
	})
	if tx.Vin[0].Txid != order.Txid || tx.Vin[0].Vout != order.Vout {
		t.Fatalf("maker input must stay at vin[0], since SIGHASH_SINGLE ties the signature to output 0; got %+v", tx.Vin[0])
	}
	if len(tx.Vin) != 2 {
		t.Fatalf("expected 2 inputs, got %d", len(tx.Vin))
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

// TestSerializeTxRejectsMalformedTxid pins that a txid must be exactly 32
// bytes. Nothing checked this: hex.DecodeString only rejects non-hex and odd
// lengths, so a 31-byte txid serialized as 31 bytes and shifted every
// following byte of the transaction by one -- outputs, values, scripts, all of
// it -- into a structurally invalid transaction that still signs and still
// hexes cleanly.
//
// This was not hypothetical. The cross-check fixture above was written with a
// 62-character txid, and because the expected bytes were generated from that
// same typo, the suite agreed with itself and stayed green. A wrong-length
// txid is never a valid transaction, so refuse to build one.
func TestSerializeTxRejectsMalformedTxid(t *testing.T) {
	for _, txid := range []string{
		"cc",
		"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",          // 31 bytes
		"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc" + "cc", // 33 bytes
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("expected a panic for a %d-character txid, got none", len(txid))
				}
			}()
			SerializeTx(Tx{Vin: []TxIn{{Txid: txid, Vout: 0}}})
		}()
	}
}
