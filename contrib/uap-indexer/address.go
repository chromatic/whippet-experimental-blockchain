package main

import (
	"crypto/sha256"
	"fmt"
	"math/big"
)

// Whippet P2PKH version bytes
const (
	MainnetVersion = 73  // Standard P2PKH version for Whippet mainnet
	TestnetVersion = 113 // Standard P2PKH version for Whippet testnet
	RegtestVersion = 73  // Standard P2PKH version for Whippet regtest
)

// Base58 alphabet used for encoding/decoding
const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// EncodeAddress renders a 20-byte hash160 as a base58check address with
// the given version byte.
func EncodeAddress(hash160 []byte, version byte) string {
	if len(hash160) != 20 {
		return ""
	}

	// Payload is version byte + hash160
	payload := make([]byte, 1+20)
	payload[0] = version
	copy(payload[1:], hash160)

	// Calculate checksum: first 4 bytes of double SHA256
	h1 := sha256.Sum256(payload)
	h2 := sha256.Sum256(h1[:])
	checksum := h2[:4]

	// Full data is payload + checksum
	fullData := append(payload, checksum...)

	// Convert to base58
	return base58Encode(fullData)
}

// DecodeAddress parses a base58check address, returning its version byte
// and 20-byte hash160, or an error if the string is malformed or the
// 4-byte double-SHA256 checksum does not match.
func DecodeAddress(addr string) (version byte, hash160 []byte, err error) {
	// Decode from base58
	data := base58Decode(addr)
	if len(data) != 1+20+4 {
		return 0, nil, fmt.Errorf("invalid address length after decoding: %d (expected %d)", len(data), 1+20+4)
	}

	// Extract payload and checksum
	payload := data[:1+20]
	checksumProvided := data[1+20:]

	// Calculate checksum
	h1 := sha256.Sum256(payload)
	h2 := sha256.Sum256(h1[:])
	checksumCalculated := h2[:4]

	// Verify checksum
	for i := 0; i < 4; i++ {
		if checksumProvided[i] != checksumCalculated[i] {
			return 0, nil, fmt.Errorf("checksum mismatch")
		}
	}

	return payload[0], payload[1:], nil
}

// base58Encode encodes data to base58
func base58Encode(data []byte) string {
	// Convert bytes to big.Int
	num := new(big.Int).SetBytes(data)

	// Convert to base58
	var result []byte
	base := big.NewInt(58)
	zero := big.NewInt(0)

	for num.Cmp(zero) > 0 {
		remainder := new(big.Int).Mod(num, base)
		result = append([]byte{base58Alphabet[remainder.Int64()]}, result...)
		num.Div(num, base)
	}

	// Add leading '1' characters for leading zero bytes
	for _, b := range data {
		if b != 0 {
			break
		}
		result = append([]byte{'1'}, result...)
	}

	if len(result) == 0 {
		return "1"
	}
	return string(result)
}

// base58Decode decodes a base58-encoded string
func base58Decode(s string) []byte {
	// Convert base58 to big.Int
	num := big.NewInt(0)
	base := big.NewInt(58)

	for _, ch := range s {
		idx := -1
		for i, c := range base58Alphabet {
			if c == ch {
				idx = i
				break
			}
		}
		if idx == -1 {
			// Character not in base58 alphabet
			return nil
		}
		num.Mul(num, base)
		num.Add(num, big.NewInt(int64(idx)))
	}

	// Convert big.Int to bytes
	result := num.Bytes()

	// Add leading zero bytes for leading '1' characters
	for _, ch := range s {
		if ch != '1' {
			break
		}
		result = append([]byte{0}, result...)
	}

	return result
}
