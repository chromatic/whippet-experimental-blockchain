package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// maxMoneySat mirrors MAX_MONEY in src/amount.h: 10,000,000,000 WHIP.
const maxMoneySat = 10000000000 * 100000000

// Amount is a quantity of satoshis decoded exactly from the decimal
// number the node puts on the wire.
//
// It exists because the obvious `float64` is wrong here. whippetd formats
// money with ValueFromAmount, which writes a full-precision decimal
// ("%d.%08d") precisely so no precision is lost in transit -- but
// encoding/json hands a bare number to float64 by default, and float64
// holds integers exactly only up to 2^53. MAX_MONEY is 1e18 satoshis, a
// hundred times past that. A position over roughly 90 million WHIP would
// be recorded off by a satoshi or more.
//
// That is not a rounding nit. A taker filling an order has to reproduce
// the position's value exactly or consensus rejects the transaction, so a
// large position indexed one satoshi light becomes silently unfillable.
type Amount int64

var errAmountRange = errors.New("amount out of range")

// UnmarshalJSON decodes a JSON number of coins into satoshis without
// going through a float. Anything that cannot be represented exactly is
// an error: a plausible-looking wrong balance is worse than a loud one.
func (a *Amount) UnmarshalJSON(b []byte) error {
	s := string(b)
	if s == "null" {
		return nil
	}
	sat, err := parseCoinsToSat(s)
	if err != nil {
		return fmt.Errorf("amount %s: %w", s, err)
	}
	*a = Amount(sat)
	return nil
}

func parseCoinsToSat(s string) (int64, error) {
	if s == "" {
		return 0, errors.New("empty")
	}
	if strings.ContainsAny(s, "eE") {
		// The node never emits exponent form. Refuse rather than guess.
		return 0, errors.New("exponent notation not supported")
	}

	neg := false
	switch s[0] {
	case '-':
		neg, s = true, s[1:]
	case '+':
		s = s[1:]
	}
	if s == "" {
		return 0, errors.New("no digits")
	}

	intPart, fracPart := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
		if fracPart == "" {
			return 0, errors.New("trailing decimal point")
		}
	}
	if intPart == "" {
		return 0, errors.New("no integer part")
	}
	if len(fracPart) > 8 {
		// Finer than a satoshi. Truncating here would be exactly the
		// silent loss this type exists to prevent.
		return 0, errors.New("more than 8 decimal places")
	}

	coins, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, errors.New("bad integer part")
	}
	var frac int64
	if fracPart != "" {
		if frac, err = strconv.ParseInt(fracPart, 10, 64); err != nil {
			return 0, errors.New("bad fractional part")
		}
		for i := len(fracPart); i < 8; i++ {
			frac *= 10
		}
	}

	// Range-check before multiplying so the multiply cannot overflow.
	if coins > maxMoneySat/100000000 {
		return 0, errAmountRange
	}
	sat := coins*100000000 + frac
	if sat > maxMoneySat {
		return 0, errAmountRange
	}
	if neg {
		sat = -sat
	}
	return sat, nil
}
