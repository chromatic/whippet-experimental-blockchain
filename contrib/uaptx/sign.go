package uaptx

import (
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// signDER produces a deterministic (RFC6979), low-S, DER-encoded ECDSA
// signature over hash using privKey. This matches what uap-js's secp.js
// signSync(hash, privKey, {canonical: true, der: true}) produces byte for
// byte: both use RFC6979 nonce derivation and BIP62 low-S normalization,
// the two properties that make ECDSA signing deterministic and
// non-malleable. See sign_test.go's TestSignDERMatchesJSFixture for the
// cross-implementation check.
func signDER(privKeyBytes []byte, hash [32]byte) []byte {
	if len(privKeyBytes) != 32 {
		panic("uaptx: private key must be exactly 32 bytes")
	}
	privKey := secp256k1.PrivKeyFromBytes(privKeyBytes)
	defer privKey.Zero()
	sig := ecdsa.Sign(privKey, hash[:])
	return sig.Serialize()
}

// SignSpend signs a spend of a UAP position. scriptCode is the exact
// scriptPubKey of the output being spent (the mint or transfer script).
// Returns the scriptSig bytes: a single push of <DER signature + sighash
// byte>.
func SignSpend(scriptCode []byte, privKey []byte, tx Tx, nIn int, hashType uint32) []byte {
	hash := SignatureHash(scriptCode, tx, nIn, hashType)
	der := signDER(privKey, hash)
	sigWithType := append(append([]byte{}, der...), byte(hashType))
	return pushData(sigWithType)
}

// SignP2PKHInput signs a P2PKH input. Returns the scriptSig bytes: a push
// of the DER signature with sighash byte, followed by a push of the public
// key. This is the standard scriptSig format for spending P2PKH outputs.
func SignP2PKHInput(scriptCode []byte, privKey []byte, pubKey []byte, tx Tx, nIn int, hashType uint32) []byte {
	hash := SignatureHash(scriptCode, tx, nIn, hashType)
	der := signDER(privKey, hash)
	sigWithType := append(append([]byte{}, der...), byte(hashType))
	var out []byte
	out = append(out, pushData(sigWithType)...)
	out = append(out, pushData(pubKey)...)
	return out
}
