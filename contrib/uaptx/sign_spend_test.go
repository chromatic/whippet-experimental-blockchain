package uaptx

// TestSignSpendMatchesJSFixture cross-checks SignSpend end-to-end (sighash
// + DER sign + scriptSig push-wrapping) against a transfer transaction
// built and signed by uap-js's buildTransferTx, byte for byte.
//
// The input is itself a transfer, so its lineage carries forward unchanged
// into the output covenant -- the expected bytes below contain that origin
// twice over (once inside the signed scriptCode, once in the new output),
// so a Go/JS disagreement about lineage handling shows up as a mismatch
// rather than as two implementations quietly agreeing on the wrong thing.
//
// Fixture generated (safe: pure local computation, no network):
//
//	cd contrib/uap-js
//	node -e '
//	  import("./secp.js").then(async (secp) => {
//	    const UAP = await import("./uap.js");
//	    UAP.configureSecp(secp);
//	    const priv = UAP.hexToBytes("33".repeat(32));
//	    const pub = secp.getPublicKey(priv, true);
//	    const origin = UAP.deriveOrigin("11".repeat(32), 7);
//	    const scriptCode = UAP.buildTransferScript(pub, 4, origin);
//	    const tx = UAP.buildTransferTx(secp, {
//	      input: { txid: "dd".repeat(32), vout: 2, scriptCode, value: 10 * UAP.COIN, privKey: priv },
//	      toPubkey: UAP.hexToBytes("03".repeat(33)),
//	      multiplier: 9,
//	      fee: 60000,
//	    });
//	    console.log(UAP.bytesToHex(tx.vin[0].scriptSig));
//	    console.log(UAP.txToHex(tx));
//	  });'

import (
	"encoding/hex"
	"testing"
)

func TestSignSpendMatchesJSFixture(t *testing.T) {
	priv := mustHex(t, "3333333333333333333333333333333333333333333333333333333333333333")
	pub := mustHex(t, "023c72addb4fdf09af94f0c94d7fe92a386a7e70cf8a1d85916386bb2535c7b1b1")

	// Derived rather than pasted: the fixture bytes below embed this origin,
	// so OriginFromOutpoint disagreeing with JS's deriveOrigin fails here too.
	origin := OriginFromOutpoint("1111111111111111111111111111111111111111111111111111111111111111", 7)
	if got, want := hex.EncodeToString(origin), "85f10ba0957bc9c0823724e945548db61b60adb811bfa4b79771ef9fc6ee0cde"; got != want {
		t.Fatalf("origin:\ngot  %s\nwant %s", got, want)
	}
	scriptCode := BuildTransferScript(pub, 4, origin)

	const fee = 60000
	const value = 10 * COIN
	toPubkey := mustHex(t, "030303030303030303030303030303030303030303030303030303030303030303")

	tx := Tx{
		Version:  1,
		Locktime: 0,
		Vin: []TxIn{{
			Txid:     "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
			Vout:     2,
			Sequence: DefaultSequence,
		}},
		Vout: []TxOut{{
			Value: value - fee,
			// A transfer's spend carries the input's lineage forward; deriving a
			// fresh origin from this transaction's own outpoint would be rejected
			// by consensus. See originForSpend in uap-js.
			ScriptPubKey: BuildTransferScript(toPubkey, 9, origin),
		}},
	}
	tx.Vin[0].ScriptSig = SignSpend(scriptCode, priv, tx, 0, SighashAll)

	wantScriptSig := "483045022100f4a7a545b8960cb29520e30610d697fb84e3aa0d9541c7f2145fa49f6052969c0220477efcaa5acd9be7870b6e35fdc5f474f05413d027dc3f2018344b574f250d0201"
	if got := hex.EncodeToString(tx.Vin[0].ScriptSig); got != wantScriptSig {
		t.Errorf("scriptSig:\ngot  %s\nwant %s", got, wantScriptSig)
	}

	wantTxHex := "0100000001dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd0200000049483045022100f4a7a545b8960cb29520e30610d697fb84e3aa0d9541c7f2145fa49f6052969c0220477efcaa5acd9be7870b6e35fdc5f474f05413d027dc3f2018344b574f250d0201ffffffff01a0df993b000000004521030303030303030303030303030303030303030303030303030303030303030303592085f10ba0957bc9c0823724e945548db61b60adb811bfa4b79771ef9fc6ee0cdeba00000000"
	if got := TxToHex(tx); got != wantTxHex {
		t.Errorf("tx hex:\ngot  %s\nwant %s", got, wantTxHex)
	}
}
