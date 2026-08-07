package main

import (
	"encoding/json"
	"math"
	"testing"
)

// The node writes every money value with ValueFromAmount, which formats
// exactly "%d.%08d" -- a full-precision decimal, never scientific
// notation. These are the shapes that actually arrive on the wire.
func TestAmountUnmarshalExact(t *testing.T) {
	cases := []struct {
		json string
		want Amount
	}{
		{`0.00000000`, 0},
		{`0.00000001`, 1},
		{`1.00000000`, 100000000},
		{`50.00000000`, 5000000000},
		{`-1.50000000`, -150000000},
		// 2^53 + 1 satoshis: the first value float64 cannot hold exactly.
		{`90071992.54740993`, 9007199254740993},
		{`1234567890.12345678`, 123456789012345678},
		// MAX_MONEY: 10,000,000,000 WHIP.
		{`10000000000.00000000`, 1000000000000000000},
		// Shapes the node does not emit but JSON permits.
		{`5`, 500000000},
		{`5.1`, 510000000},
	}
	for _, c := range cases {
		var got Amount
		if err := json.Unmarshal([]byte(c.json), &got); err != nil {
			t.Fatalf("%s: unexpected error: %v", c.json, err)
		}
		if got != c.want {
			t.Errorf("%s: got %d sat, want %d sat (off by %d)", c.json, got, c.want, int64(got-c.want))
		}
	}
}

// The whole point of the type: the values above must survive a round trip
// that float64 provably mangles. If this test ever passes with a float64
// implementation, it is not testing anything.
func TestAmountBeatsFloat64(t *testing.T) {
	const raw = `1234567890.12345678`
	const want = Amount(123456789012345678)

	var exact Amount
	if err := json.Unmarshal([]byte(raw), &exact); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exact != want {
		t.Fatalf("exact parse got %d, want %d", exact, want)
	}

	var loose float64
	if err := json.Unmarshal([]byte(raw), &loose); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if viaFloat := int64(math.Round(loose * 1e8)); viaFloat == int64(want) {
		t.Fatal("float64 round-trip was exact for this vector; pick a harder one")
	}
}

func TestAmountUnmarshalRejects(t *testing.T) {
	// Anything we cannot represent exactly must be an error rather than a
	// silent rounding. A wrong balance that looks plausible is worse than
	// a loud failure.
	for _, bad := range []string{
		`1.234567891`,           // 9 decimals: finer than a satoshi
		`1e9`,                   // exponent form
		`1.5e3`,                 // exponent form
		`"1.0"`,                 // string, not a number
		`abc`,                   // not a number at all
		``,                      // empty
		`.5`,                    // no integer part
		`1.`,                    // trailing point
		`--1`,                   // malformed sign
		`100000000000.00000000`, // 100 billion WHIP: above MAX_MONEY
	} {
		var got Amount
		if err := json.Unmarshal([]byte(bad), &got); err == nil {
			t.Errorf("%q: expected an error, got %d", bad, got)
		}
	}
}

// A vout carries its value through the same path, so the type has to work
// where it is actually used, not just in isolation.
func TestRPCVoutValueIsExact(t *testing.T) {
	const raw = `{"value":90071992.54740993,"n":3,"scriptPubKey":{"hex":"deadbeef"}}`
	var v RPCVout
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Value != 9007199254740993 {
		t.Errorf("got %d sat, want 9007199254740993 sat", v.Value)
	}
	if v.N != 3 || v.ScriptPubKey.Hex != "deadbeef" {
		t.Errorf("surrounding fields did not survive: %+v", v)
	}
}
