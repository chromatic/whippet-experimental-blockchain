package uaptx

import "crypto/sha256"

// sighashDegenerateHash is the classic Satoshi-client degenerate case:
// SignatureHash() returns the 256-bit value 1 (little-endian byte order:
// 0x01 followed by 31 zero bytes) rather than erroring, for nIn out of
// range or SIGHASH_SINGLE with no matching output. Preserved for exact
// consensus compatibility with SignatureHash() in
// src/script/interpreter.cpp.
var sighashDegenerateHash = func() [32]byte {
	var h [32]byte
	h[0] = 1
	return h
}()

func hash256(data []byte) [32]byte {
	h1 := sha256.Sum256(data)
	h2 := sha256.Sum256(h1[:])
	return h2
}

// SignatureHash computes the legacy (pre-segwit) SignatureHash, matching
// SignatureHash() in src/script/interpreter.cpp for SIGVERSION_BASE.
// Supports all four base types (ALL/NONE/SINGLE) combined with the
// ANYONECANPAY flag. scriptCode is the exact scriptPubKey (or relevant
// portion) of the output being spent by input nIn.
func SignatureHash(scriptCode []byte, tx Tx, nIn int, hashType uint32) [32]byte {
	baseType := hashType & 0x1f
	anyoneCanPay := hashType&SighashAnyoneCanPay != 0

	if nIn >= len(tx.Vin) {
		return sighashDegenerateHash
	}
	if baseType == SighashSingle && nIn >= len(tx.Vout) {
		return sighashDegenerateHash
	}

	signedScript := serializeScriptCode(scriptCode)

	var inputIndices []int
	if anyoneCanPay {
		inputIndices = []int{nIn}
	} else {
		inputIndices = make([]int, len(tx.Vin))
		for i := range tx.Vin {
			inputIndices[i] = i
		}
	}

	vinOut := make([]TxIn, len(inputIndices))
	for idx, i := range inputIndices {
		vin := tx.Vin[i]
		isSigned := i == nIn
		zeroSequence := !isSigned && (baseType == SighashSingle || baseType == SighashNone)

		out := TxIn{
			Txid: vin.Txid,
			Vout: vin.Vout,
		}
		if isSigned {
			out.ScriptSig = signedScript.bytes
			dl := signedScript.declaredLength
			out.scriptSigLength = &dl
		} else {
			out.ScriptSig = []byte{}
		}
		if zeroSequence {
			out.Sequence = 0
		} else {
			out.Sequence = vin.Sequence
		}
		vinOut[idx] = out
	}

	var voutOut []TxOut
	switch baseType {
	case SighashNone:
		voutOut = []TxOut{}
	case SighashSingle:
		// Null (value=-1, empty script) placeholders for every output
		// before nIn, then the one real output being pinned. Outputs
		// after nIn are dropped entirely (not committed to at all).
		voutOut = make([]TxOut, 0, nIn+1)
		for i := 0; i < nIn; i++ {
			voutOut = append(voutOut, TxOut{Value: -1, ScriptPubKey: []byte{}})
		}
		voutOut = append(voutOut, tx.Vout[nIn])
	default:
		voutOut = tx.Vout
	}

	tmp := Tx{Version: tx.Version, Locktime: tx.Locktime, Vin: vinOut, Vout: voutOut}
	preimage := append(SerializeTx(tmp), encodeUint32LE(hashType)...)
	return hash256(preimage)
}
