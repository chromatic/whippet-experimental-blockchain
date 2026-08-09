package uaptx

// TestSignSpendMatchesJSFixture cross-checks SignSpend end-to-end (sighash
// + DER sign + scriptSig push-wrapping) against a transfer transaction
// built and signed by uap-js's buildTransferTx, byte for byte.
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
//	    const scriptCode = UAP.buildTransferScript(pub, 4);
//	    const tx = UAP.buildTransferTx(secp, {
//	      input: { txid: "dd".repeat(32), vout: 2, scriptCode, value: 10 * UAP.COIN, privKey: priv },
//	      toPubkey: UAP.hexToBytes("03".repeat(33)),
//	      multiplier: 9,
//	      fee: 60000,
//	    });
//	    console.log(UAP.txToHex(tx));
//	  });'

import (
	"encoding/hex"
	"testing"
)

func TestSignSpendMatchesJSFixture(t *testing.T) {
	priv := mustHex(t, "3333333333333333333333333333333333333333333333333333333333333333")
	pub := mustHex(t, "023c72addb4fdf09af94f0c94d7fe92a386a7e70cf8a1d85916386bb2535c7b1b1")
	scriptCode := BuildTransferScript(pub, 4)

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
			Value:        value - fee,
			ScriptPubKey: BuildTransferScript(toPubkey, 9),
		}},
	}
	tx.Vin[0].ScriptSig = SignSpend(scriptCode, priv, tx, 0, SighashAll)

	wantScriptSig := "47304402200dcf6f3e68ba39456e8dcb5403b0d0564e04c9dade1869e8e7166da68dfe314b022023666f78b7670205522220514453809c6164247698cca1c0127f16d088f4da5401"
	if got := hex.EncodeToString(tx.Vin[0].ScriptSig); got != wantScriptSig {
		t.Errorf("scriptSig:\ngot  %s\nwant %s", got, wantScriptSig)
	}

	wantTxHex := "0100000001dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd020000004847304402200dcf6f3e68ba39456e8dcb5403b0d0564e04c9dade1869e8e7166da68dfe314b022023666f78b7670205522220514453809c6164247698cca1c0127f16d088f4da5401ffffffff01a0df993b00000000242103030303030303030303030303030303030303030303030303030303030303030359ba00000000"
	if got := TxToHex(tx); got != wantTxHex {
		t.Errorf("tx hex:\ngot  %s\nwant %s", got, wantTxHex)
	}
}
