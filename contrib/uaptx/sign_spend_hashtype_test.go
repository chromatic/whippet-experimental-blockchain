package uaptx

// TestSignSpendRespectsHashType cross-checks SignSpend with a non-default
// hashType (SIGHASH_SINGLE|SIGHASH_ANYONECANPAY, the exact combination
// uap-js's signMakerOrder uses for maker orders) against a JS-generated
// fixture, at input index 1 of a 2-input transaction -- so this also
// pins the input index actually being honoured, not just the hashType.
//
// Fixture generated (safe: pure local computation, no network):
//
//	cd contrib/uap-js
//	node -e '
//	  import("./secp.js").then(async (secp) => {
//	    const UAP = await import("./uap.js");
//	    UAP.configureSecp(secp);
//	    const priv = UAP.hexToBytes("44".repeat(32));
//	    const pub = secp.getPublicKey(priv, true);
//	    const scriptCode = UAP.buildTransferScript(pub, 2);
//	    const tx = {
//	      version: 1, locktime: 0,
//	      vin: [
//	        { txid: "ee".repeat(32), vout: 0, scriptSig: new Uint8Array(0), sequence: 0xffffffff },
//	        { txid: "ff".repeat(32), vout: 1, scriptSig: new Uint8Array(0), sequence: 0xffffffff },
//	      ],
//	      vout: [{ value: 5000, scriptPubKey: UAP.hexToBytes("51") }],
//	    };
//	    const hashType = UAP.SIGHASH_SINGLE | UAP.SIGHASH_ANYONECANPAY;
//	    const scriptSig = UAP.signSpend(secp, scriptCode, priv, tx, 1, hashType);
//	    console.log(UAP.bytesToHex(scriptSig));
//	  });'

import (
	"encoding/hex"
	"testing"
)

func TestSignSpendRespectsHashType(t *testing.T) {
	priv := mustHex(t, "4444444444444444444444444444444444444444444444444444444444444444")
	pub := mustHex(t, "032c0b7cf95324a07d05398b240174dc0c2be444d96b159aa6c7f7b1e668680991")
	scriptCode := BuildTransferScript(pub, 2)

	tx := Tx{
		Version:  1,
		Locktime: 0,
		Vin: []TxIn{
			{Txid: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", Vout: 0, Sequence: DefaultSequence},
			{Txid: "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", Vout: 1, Sequence: DefaultSequence},
		},
		Vout: []TxOut{{Value: 5000, ScriptPubKey: []byte{0x51}}},
	}

	hashType := uint32(SighashSingle | SighashAnyoneCanPay)
	got := SignSpend(scriptCode, priv, tx, 1, hashType)
	want := "4730440220256d7fcea7aa7bef15c4b6ae8ee086ad4a58904d48567819a4ae741a1b1bd1400220203c491f741ac6391e635a6869e971ffa06a78cf572c616de87b79c534fe0ec683"
	if gotHex := hex.EncodeToString(got); gotHex != want {
		t.Errorf("got  %s\nwant %s", gotHex, want)
	}
}
