package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// RPCClient is a minimal JSON-RPC client for whippetd, using only the
// standard library so this stays a single dependency-free binary.
type RPCClient struct {
	url        string
	user, pass string
	http       *http.Client
}

func NewRPCClient(host string, port int, user, pass, cookieFile string) (*RPCClient, error) {
	if cookieFile != "" {
		data, err := os.ReadFile(cookieFile)
		if err != nil {
			return nil, fmt.Errorf("reading rpc cookie file: %w", err)
		}
		parts := strings.SplitN(strings.TrimSpace(string(data)), ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("malformed rpc cookie file %s", cookieFile)
		}
		user, pass = parts[0], parts[1]
	}
	return &RPCClient{
		url:  fmt.Sprintf("http://%s:%d/", host, port),
		user: user,
		pass: pass,
		http: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

type rpcRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      string        `json:"id"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

func (c *RPCClient) call(method string, params []interface{}, out interface{}) error {
	body, err := json.Marshal(rpcRequest{JSONRPC: "1.0", ID: "uap-indexer", Method: method, Params: params})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.user != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("rpc request %s: %w", method, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if os.Getenv("UAP_RPC_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "DEBUG request: %s\nDEBUG response: %s\n", body, data)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusInternalServerError {
		return fmt.Errorf("rpc %s: unexpected HTTP status %d: %s", method, resp.StatusCode, string(data))
	}
	var rr rpcResponse
	if err := json.Unmarshal(data, &rr); err != nil {
		return fmt.Errorf("rpc %s: decoding response: %w", method, err)
	}
	if rr.Error != nil {
		return fmt.Errorf("rpc %s: %s (code %d)", method, rr.Error.Message, rr.Error.Code)
	}
	if out != nil {
		return json.Unmarshal(rr.Result, out)
	}
	return nil
}

func (c *RPCClient) GetBlockCount() (int64, error) {
	var height int64
	err := c.call("getblockcount", nil, &height)
	return height, err
}

func (c *RPCClient) GetBlockHash(height int64) (string, error) {
	var hash string
	err := c.call("getblockhash", []interface{}{height}, &hash)
	return hash, err
}

// RPCVin is one transaction input as returned by getblock verbosity=2 / getrawtransaction.
type RPCVin struct {
	TxID string `json:"txid"`
	Vout uint32 `json:"vout"`
	// Coinbase is set (to arbitrary script hex) instead of TxID/Vout for coinbase inputs.
	Coinbase string `json:"coinbase"`
}

// RPCVout is one transaction output.
type RPCVout struct {
	Value        float64 `json:"value"`
	N            uint32  `json:"n"`
	ScriptPubKey struct {
		Hex string `json:"hex"`
	} `json:"scriptPubKey"`
}

// RPCTx is a transaction as embedded in getblock verbosity=2.
type RPCTx struct {
	TxID string    `json:"txid"`
	Vin  []RPCVin  `json:"vin"`
	Vout []RPCVout `json:"vout"`
}

// RPCBlock is a block as returned by getblock verbosity=2.
type RPCBlock struct {
	Hash              string  `json:"hash"`
	Height            int64   `json:"height"`
	PreviousBlockHash string  `json:"previousblockhash"`
	Tx                []RPCTx `json:"tx"`
}

func (c *RPCClient) GetBlockVerbose(hash string) (*RPCBlock, error) {
	var block RPCBlock
	err := c.call("getblock", []interface{}{hash, 2}, &block)
	return &block, err
}
