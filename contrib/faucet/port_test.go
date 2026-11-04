package main

import "testing"

func TestEffectiveRPCPort(t *testing.T) {
	cases := []struct {
		testnet, portSet bool
		port, want       int
	}{
		{false, false, mainnetRPCPort, 33665},
		{true, false, mainnetRPCPort, 43665},
		{true, true, 12345, 12345},
		{false, true, 12345, 12345},
	}
	for _, c := range cases {
		if got := effectiveRPCPort(c.testnet, c.portSet, c.port); got != c.want {
			t.Errorf("effectiveRPCPort(%v, %v, %d) = %d, want %d", c.testnet, c.portSet, c.port, got, c.want)
		}
	}
}
