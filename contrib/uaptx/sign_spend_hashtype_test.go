package uaptx

// Cross-checks of SignSpend under SIGHASH_SINGLE|SIGHASH_ANYONECANPAY --
// the exact combination uap-js's signMakerOrder uses for maker orders --
// against JS-generated fixtures, at input index 1 of a 2-input
// transaction, so the input index actually being honoured is pinned too.
//
// The transaction has TWO outputs on purpose. With only one, nIn=1 is out
// of range for SIGHASH_SINGLE and the sighash degenerates to the constant
// uint256(1) -- Bitcoin's long-standing SIGHASH_SINGLE bug, which Whippet
// inherits. A signature over that constant is independent of the
// scriptCode, the inputs and the outputs alike, so a fixture built that
// way pins nothing whatsoever about the sighash algorithm. The previous
// version of this test was exactly that: it kept passing across a covenant
// format change that rewrote every scriptCode it claimed to sign.
//
// TestSignSpendSighashSingleOutOfRange below covers the degenerate case
// deliberately, which is where it belongs.
//
// Fixtures generated (safe: pure local computation, no network):
//
//	cd contrib/uap-js
//	node -e '
//	  import("./secp.js").then(async (secp) => {
//	    const UAP = await import("./uap.js");
//	    UAP.configureSecp(secp);
//	    const priv = UAP.hexToBytes("44".repeat(32));
//	    const pub = secp.getPublicKey(priv, true);
//	    const origin = UAP.deriveOrigin("22".repeat(32), 1);
//	    const scriptCode = UAP.buildTransferScript(pub, 2, origin);
//	    const vin = [
//	      { txid: "ee".repeat(32), vout: 0, scriptSig: new Uint8Array(0), sequence: 0xffffffff },
//	      { txid: "ff".repeat(32), vout: 1, scriptSig: new Uint8Array(0), sequence: 0xffffffff },
//	    ];
//	    const ht = UAP.SIGHASH_SINGLE | UAP.SIGHASH_ANYONECANPAY;
//	    const two = { version: 1, locktime: 0, vin, vout: [
//	      { value: 5000, scriptPubKey: UAP.hexToBytes("51") },
//	      { value: 6000, scriptPubKey: UAP.buildTransferScript(UAP.hexToBytes("03".repeat(33)), 2, origin) },
//	    ]};
//	    const one = { version: 1, locktime: 0, vin, vout: two.vout.slice(0, 1) };
//	    console.log(UAP.bytesToHex(UAP.signatureHash(scriptCode, two, 1, ht)));
//	    console.log(UAP.bytesToHex(UAP.signSpend(secp, scriptCode, priv, two, 1, ht)));
//	    console.log(UAP.bytesToHex(UAP.signSpend(secp, scriptCode, priv, two, 0, ht)));
//	    console.log(UAP.bytesToHex(UAP.signSpend(secp, scriptCode, priv, one, 1, ht)));
//	  });'

import (
	"encoding/hex"
	"testing"
)

// hashTypeFixture rebuilds the transaction the fixtures above were
// generated against.
func hashTypeFixture(t *testing.T, nOut int) (scriptCode []byte, priv []byte, tx Tx) {
	t.Helper()
	priv = mustHex(t, "4444444444444444444444444444444444444444444444444444444444444444")
	pub := mustHex(t, "032c0b7cf95324a07d05398b240174dc0c2be444d96b159aa6c7f7b1e668680991")

	origin := OriginFromOutpoint("2222222222222222222222222222222222222222222222222222222222222222", 1)
	if got, want := hex.EncodeToString(origin), "0de51d36293ff3db39d749e0f84dcb0271b5d726729ab707d6c5922df0f27d41"; got != want {
		t.Fatalf("origin:\ngot  %s\nwant %s", got, want)
	}
	scriptCode = BuildTransferScript(pub, 2, origin)

	vout := []TxOut{
		{Value: 5000, ScriptPubKey: []byte{0x51}},
		{Value: 6000, ScriptPubKey: BuildTransferScript(mustHex(t, "030303030303030303030303030303030303030303030303030303030303030303"), 2, origin)},
	}
	tx = Tx{
		Version:  1,
		Locktime: 0,
		Vin: []TxIn{
			{Txid: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", Vout: 0, Sequence: DefaultSequence},
			{Txid: "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", Vout: 1, Sequence: DefaultSequence},
		},
		Vout: vout[:nOut],
	}
	return scriptCode, priv, tx
}

func TestSignSpendRespectsHashType(t *testing.T) {
	scriptCode, priv, tx := hashTypeFixture(t, 2)
	hashType := uint32(SighashSingle | SighashAnyoneCanPay)

	wantHash := "4fdf33a19089aa29f92510d0e34d80635e220b226e4160a3ee06fc752597d502"
	h := SignatureHash(scriptCode, tx, 1, hashType)
	if got := hex.EncodeToString(h[:]); got != wantHash {
		t.Errorf("sighash:\ngot  %s\nwant %s", got, wantHash)
	}

	want := "483045022100f88f7d7d64eac6df808c7d990737aa78e3b6e90b5bf12de6e1c53f427a05e96b02200530b2a94148dee410b34e5d8fcfec80ab7d76df34c66305c407ea79994a1a8583"
	if got := hex.EncodeToString(SignSpend(scriptCode, priv, tx, 1, hashType)); got != want {
		t.Errorf("scriptSig:\ngot  %s\nwant %s", got, want)
	}
}

// TestSignSpendHonoursInputIndex signs the same transaction at input 0
// instead of input 1. Under SIGHASH_SINGLE the two commit to different
// outputs, so a SignSpend that ignored nIn would produce the fixture above
// here as well.
func TestSignSpendHonoursInputIndex(t *testing.T) {
	scriptCode, priv, tx := hashTypeFixture(t, 2)
	hashType := uint32(SighashSingle | SighashAnyoneCanPay)

	want := "47304402205098be7f6bf7afae5a70a348380e733d273d2a989f9174ad59e1b1d2da78df850220062c6d94d861eb71d165b2493ff91d83ea2c36e513b373f8b7c9489aba0a54db83"
	if got := hex.EncodeToString(SignSpend(scriptCode, priv, tx, 0, hashType)); got != want {
		t.Errorf("scriptSig at input 0:\ngot  %s\nwant %s", got, want)
	}
}

// TestSignSpendSighashSingleOutOfRange pins the SIGHASH_SINGLE bug: when
// nIn has no matching output, the sighash is the constant uint256(1)
// rather than an error. Both implementations must reproduce it, because a
// node that does anything else would reject transactions the other builds.
func TestSignSpendSighashSingleOutOfRange(t *testing.T) {
	scriptCode, priv, tx := hashTypeFixture(t, 1)
	hashType := uint32(SighashSingle | SighashAnyoneCanPay)

	h := SignatureHash(scriptCode, tx, 1, hashType)
	wantHash := "0100000000000000000000000000000000000000000000000000000000000000"
	if got := hex.EncodeToString(h[:]); got != wantHash {
		t.Errorf("out-of-range sighash:\ngot  %s\nwant %s", got, wantHash)
	}

	want := "4730440220256d7fcea7aa7bef15c4b6ae8ee086ad4a58904d48567819a4ae741a1b1bd1400220203c491f741ac6391e635a6869e971ffa06a78cf572c616de87b79c534fe0ec683"
	if got := hex.EncodeToString(SignSpend(scriptCode, priv, tx, 1, hashType)); got != want {
		t.Errorf("scriptSig:\ngot  %s\nwant %s", got, want)
	}
}
