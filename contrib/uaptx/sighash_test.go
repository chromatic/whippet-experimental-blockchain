package uaptx

// What the C++ fixture cannot reach: 0xab inside a data push. RandomScript
// in sighash_tests.cpp only ever emits single-byte opcodes, so not one of
// the 500 vectors contains a data push -- leaving the most dangerous half
// of the stripping rule untested by them: 0xab is only an OP_CODESEPARATOR
// when it appears as an opcode; the same byte inside a push is data, and
// removing it would silently sign a different script than the one being
// spent. Mirrors the corresponding section of
// contrib/uap-js/sighash-vectors.test.js. The oracle here is not this
// implementation -- a scriptCode with no OP_CODESEPARATOR *opcode* must be
// signed verbatim, so the preimage is exactly SerializeTx of the
// transaction with that script spliced into the signed input, built
// independently below and itself pinned by the 500 round-trips in
// sighash_vectors_test.go.

import (
	"encoding/hex"
	"testing"
)

func baseSighashTestTx(scriptSig []byte) Tx {
	return Tx{
		Version:  1,
		Locktime: 0,
		Vin: []TxIn{{
			Txid:      "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Vout:      0,
			ScriptSig: scriptSig,
			Sequence:  0xffffffff,
		}},
		Vout: []TxOut{{Value: 1000, ScriptPubKey: []byte{0x51}}},
	}
}

func TestSignatureHashDoesNotStripCodeSeparatorByteInsideAPush(t *testing.T) {
	cases := map[string]string{
		"a 2-byte push of 0xab 0xab":        "02abab",
		"OP_PUSHDATA1 carrying 0xab":        "4c03ababab",
		"OP_PUSHDATA2 carrying 0xab":        "4d0200abab",
		"a push of 0xab next to a real one": "01ab",
	}
	for what, hexStr := range cases {
		scriptCode, err := hex.DecodeString(hexStr)
		if err != nil {
			t.Fatalf("%s: bad hex: %v", what, err)
		}
		tx := baseSighashTestTx(nil)
		expectedTx := baseSighashTestTx(scriptCode)
		preimage := append(SerializeTx(expectedTx), encodeUint32LE(SighashAll)...)
		expected := hash256(preimage)

		got := SignatureHash(scriptCode, tx, 0, SighashAll)
		if got != expected {
			t.Errorf("%s must be signed verbatim -- 0xab is data here, not an opcode: got %x want %x", what, got, expected)
		}
	}
}

func TestSignatureHashStripsTrailingCodeSeparatorOpcode(t *testing.T) {
	tx := baseSighashTestTx(nil)
	bare, _ := hex.DecodeString("51")
	separated, _ := hex.DecodeString("51ab")

	gotBare := SignatureHash(bare, tx, 0, SighashAll)
	gotSeparated := SignatureHash(separated, tx, 0, SighashAll)
	if gotBare != gotSeparated {
		t.Errorf("a trailing OP_CODESEPARATOR opcode must vanish from the preimage: bare=%x separated=%x", gotBare, gotSeparated)
	}
}
