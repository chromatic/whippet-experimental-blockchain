package main

import "testing"

// A cancel signature produced by contrib/uap-js must verify here. The two
// implementations build cancelMessage independently, in different
// languages, and each side's own tests pass whether or not they agree with
// each other -- so without this vector a divergence would ship silently and
// cancellation would simply never work in production.
//
// Regenerate with:
//
//	cd contrib/uap-js && node -e "import('./uap.js').then(async (uap) => {
//	  const secp = await import('./secp.js'); uap.configureSecp(secp);
//	  const priv = new Uint8Array(32).fill(0x2b);
//	  const args = { txid: 'ab'.repeat(32), vout: 3,
//	                 scriptSig: '47304402deadbeef',
//	                 cancelNonce: '1754576000123456789' };
//	  console.log(uap.signCancelOrder(secp, { ...args, privKey: priv })); });"
func TestCancelSignatureFromJavaScriptVerifies(t *testing.T) {
	const (
		pubKey    = "02bb58b5feca505c74edc000d8282fc556e51a1024fc8e7d7e56c6f887c5c8d5f2"
		txid      = "abababababababababababababababababababababababababababababababab"
		vout      = uint32(3)
		scriptSig = "47304402deadbeef"
		nonce     = int64(1754576000123456789)
		sig       = "304402203ac4261db9bd4d30cce46d0df434287350c7f897fb0ab9e6bfda8d6a0328b0ce022066b0c539274e965223cc36e2afefbc7d53f2c06afd2d2044a9939a81454d3336"
	)

	if err := verifyCancelSignature(pubKey, txid, vout, scriptSig, nonce, sig); err != nil {
		t.Fatalf("signature from uap-js rejected: %v", err)
	}

	// The vector must be pinned to its own message, not merely parseable.
	if err := verifyCancelSignature(pubKey, txid, vout+1, scriptSig, nonce, sig); err == nil {
		t.Error("signature verified against a different vout")
	}
	if err := verifyCancelSignature(pubKey, txid, vout, scriptSig, nonce+1, sig); err == nil {
		t.Error("signature verified against a different cancel_nonce")
	}
	if err := verifyCancelSignature(pubKey, txid, vout, scriptSig+"00", nonce, sig); err == nil {
		t.Error("signature verified against a different script_sig")
	}
}
