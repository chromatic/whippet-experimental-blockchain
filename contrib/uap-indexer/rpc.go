package main

import (
	"net/http"

	"github.com/whippet/whippet/contrib/whippetrpc"
)

// NodeRejection is an error the node itself returned: the RPC call reached
// whippetd, and whippetd said no. It is distinct from a transport failure,
// which means the call never got an answer at all.
//
// The distinction matters at the HTTP layer. Telling a user their perfectly
// valid transaction was rejected, when in fact the node was unreachable,
// sends them off debugging a transaction that was never seen. Callers use
// errors.As to tell the two apart -- never a substring match on the message,
// which silently misclassifies as soon as wording changes.
type NodeRejection = whippetrpc.NodeRejection

// RPCClient is the indexer's view of whippetd. The transport lives in
// contrib/whippetrpc, shared with contrib/faucet; only the methods the
// indexer calls are here.
//
// The connection details stay as plain fields rather than a wrapped client so
// that a test can point one at an httptest server with a struct literal.
type RPCClient struct {
	url        string
	user, pass string
	http       *http.Client
}

func NewRPCClient(host string, port int, user, pass, cookieFile string) (*RPCClient, error) {
	c, err := whippetrpc.New(whippetrpc.Config{
		Host:       host,
		Port:       port,
		User:       user,
		Pass:       pass,
		CookieFile: cookieFile,
		ID:         rpcID,
		DebugEnv:   rpcDebugEnv,
	})
	if err != nil {
		return nil, err
	}
	return &RPCClient{
		url:  c.URL(),
		user: c.User(),
		pass: c.Pass(),
		http: c.HTTPClient(),
	}, nil
}

const (
	rpcID       = "uap-indexer"
	rpcDebugEnv = "UAP_RPC_DEBUG"
)

func (c *RPCClient) call(method string, params []interface{}, out interface{}) error {
	rpc, err := whippetrpc.New(whippetrpc.Config{
		URL:      c.url,
		User:     c.user,
		Pass:     c.pass,
		ID:       rpcID,
		DebugEnv: rpcDebugEnv,
		HTTP:     c.http,
	})
	if err != nil {
		return err
	}
	return rpc.Call(method, params, out)
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
	// Value is satoshis, decoded exactly -- see amount.go for why this
	// must not be a float64.
	Value        Amount `json:"value"`
	N            uint32 `json:"n"`
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

// SendRawTransaction broadcasts a raw transaction to the network.
// Returns the txid on success, or an error (which may be a node rejection).
func (c *RPCClient) SendRawTransaction(hex string) (string, error) {
	var txid string
	err := c.call("sendrawtransaction", []interface{}{hex}, &txid)
	return txid, err
}

// EstimateSmartFee returns an estimated fee rate in satoshis per kB for
// confirmation within the given number of blocks, or an error.
// Returns 0 if the node cannot estimate (e.g., not enough mempool history).
func (c *RPCClient) EstimateSmartFee(blocks int) (int64, error) {
	type esf struct {
		FeRate Amount   `json:"feerate"`
		Errors []string `json:"errors"`
	}
	var resp esf
	err := c.call("estimatesmartfee", []interface{}{blocks}, &resp)
	if err != nil {
		return 0, err
	}
	// FeRate arrives as coins per kB and is decoded straight to satoshis.
	return int64(resp.FeRate), nil
}

// GetRawMempool returns the list of txids currently in the node's mempool.
func (c *RPCClient) GetRawMempool() ([]string, error) {
	var txids []string
	err := c.call("getrawmempool", []interface{}{false}, &txids)
	return txids, err
}

// GetRawTransaction returns a single transaction from the mempool or blockchain
// with full input/output details (verbosity 1).
func (c *RPCClient) GetRawTransaction(txid string) (*RPCTx, error) {
	// This will return both confirmed and unconfirmed transactions.
	var tx RPCTx
	err := c.call("getrawtransaction", []interface{}{txid, 1}, &tx)
	return &tx, err
}

// GetRawTransactionHex returns the exact serialized bytes of a transaction,
// hex-encoded, from the mempool or blockchain (verbosity 0).
//
// This is deliberately not GetRawTransaction with a .Hex field bolted on.
// The whole point of serving raw hex to a caller who is about to hash it
// (see /rawtx in api.go) is that the bytes are the ones the txid actually
// commits to -- verbosity 1's decoded JSON is whippetd's own re-rendering
// of the transaction and is not what hashes to the txid. Keeping this as a
// separate call makes it impossible to wire the API handler to the wrong
// one by accident.
func (c *RPCClient) GetRawTransactionHex(txid string) (string, error) {
	var hex string
	err := c.call("getrawtransaction", []interface{}{txid, 0}, &hex)
	return hex, err
}
