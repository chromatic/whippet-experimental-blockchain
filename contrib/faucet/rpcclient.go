package main

import (
	"github.com/whippet/whippet/contrib/whippetrpc"
)

// NodeRejection is an error whippetd itself returned: the call arrived and
// the node said no, as opposed to a transport failure where the call never
// got an answer. Use errors.As to tell the two apart.
type NodeRejection = whippetrpc.NodeRejection

// RPCClient implements NodeClient by communicating with whippetd via
// JSON-RPC. The transport lives in contrib/whippetrpc, shared with
// contrib/uap-indexer; only the faucet's own methods are here.
type RPCClient struct {
	rpc *whippetrpc.Client
}

// NewRPCClient creates a new JSON-RPC client for whippetd.
// It supports three ways to authenticate:
// 1. Explicit user/pass (passed as arguments)
// 2. RPC cookie file (parsed from path, overrides user/pass)
// 3. No authentication (user/pass are empty)
func NewRPCClient(host string, port int, user, pass, cookieFile string) (*RPCClient, error) {
	rpc, err := whippetrpc.New(whippetrpc.Config{
		Host:       host,
		Port:       port,
		User:       user,
		Pass:       pass,
		CookieFile: cookieFile,
		ID:         "faucet",
		DebugEnv:   "FAUCET_RPC_DEBUG",
	})
	if err != nil {
		return nil, err
	}
	return &RPCClient{rpc: rpc}, nil
}

// ValidateAddress calls the RPC validateaddress method and returns whether the address is valid.
func (c *RPCClient) ValidateAddress(address string) (bool, error) {
	type validateAddressResult struct {
		IsValid bool `json:"isvalid"`
	}
	var result validateAddressResult
	err := c.rpc.Call("validateaddress", []interface{}{address}, &result)
	return result.IsValid, err
}

// SendToAddress calls the RPC sendtoaddress method and returns the transaction ID.
func (c *RPCClient) SendToAddress(address string, amount float64) (string, error) {
	var txid string
	err := c.rpc.Call("sendtoaddress", []interface{}{address, amount}, &txid)
	return txid, err
}
