package uaptx

// TestSignDERMatchesJSFixture cross-checks signDER against DER signatures
// produced by contrib/uap-js's secp.js (via @noble/secp256k1 v1-compatible
// signSync, canonical/low-S + DER). This is what proves the two
// implementations' choice of nonce (RFC6979) and S-normalization (BIP62
// low-S) actually agree byte for byte -- not just "both claim to be
// canonical".
//
// Fixture generation (safe: pure local computation, no network, no key
// touches anything real):
//
//	cd contrib/uap-js
//	node -e '
//	  import("./secp.js").then(async (secp) => {
//	    const UAP = await import("./uap.js");
//	    const priv = UAP.hexToBytes("<hex>");
//	    const hash = UAP.hexToBytes("<hex>");
//	    const der = secp.signSync(hash, priv, { canonical: true, der: true });
//	    console.log(UAP.bytesToHex(der));
//	  });'

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestSignDERMatchesJSFixture(t *testing.T) {
	cases := []struct {
		name    string
		privHex string
		hashHex string
		wantDER string
	}{
		{
			name:    "priv=1, hash=0",
			privHex: "0000000000000000000000000000000000000000000000000000000000000001",
			hashHex: "0000000000000000000000000000000000000000000000000000000000000000",
			wantDER: "3045022100a0b37f8fba683cc68f6574cd43b39f0343a50008bf6ccea9d13231d9e7e2e1e4022011edc8d307254296264aebfc3dc76cd8b668373a072fd64665b50000e9fcce52",
		},
		{
			name:    "priv=0x1111...11, hash=0xaaaa...aa",
			privHex: "1111111111111111111111111111111111111111111111111111111111111111",
			hashHex: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			wantDER: "3045022100bb70e40ef81d63acdb3e71bb415b10459cb4a2f9cfa589785aad71900d3a7c23022074e8969eab72eb7a8d2fadd7987e5eb6e3faa4212ef35bd2916345efc4f039b9",
		},
		{
			name:    "priv=0xbbbb...bb, hash=0x0101...01",
			privHex: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			hashHex: "0101010101010101010101010101010101010101010101010101010101010101",
			wantDER: "3045022100dbd2ecca2802fdc003ae2657cb58bb999a80d62846178e4795716534bc5a269d0220789836b81ff25e6c880cd681d44095260730b31ed298fc51e0a97f8d8f624cca",
		},
		{
			name:    "priv=0x4242...42, hash=deadbeef repeated",
			privHex: "4242424242424242424242424242424242424242424242424242424242424242",
			hashHex: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			wantDER: "3045022100837325b31469d25e093a4ccd1267dfa84bc2a68f5d9ad961279194b8bf51b4ed02204ab33a9f473f258f31252296e45a64e8f734f520eb17221fa02e2d67e3887719",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			priv, err := hex.DecodeString(c.privHex)
			if err != nil {
				t.Fatalf("bad priv hex: %v", err)
			}
			hashBytes, err := hex.DecodeString(c.hashHex)
			if err != nil {
				t.Fatalf("bad hash hex: %v", err)
			}
			var hash [32]byte
			copy(hash[:], hashBytes)

			got := signDER(priv, hash)
			want, err := hex.DecodeString(c.wantDER)
			if err != nil {
				t.Fatalf("bad want hex: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("got  %x\nwant %x", got, want)
			}
		})
	}
}
