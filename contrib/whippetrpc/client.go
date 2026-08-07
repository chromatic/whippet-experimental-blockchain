// Package whippetrpc is a minimal JSON-RPC client for whippetd.
//
// It is transport only: it knows how to reach a node, authenticate, send a
// call and classify the answer. It deliberately knows nothing about any
// particular RPC method, so callers keep their own typed wrappers
// (GetBlockCount, SendToAddress, ...) in their own packages.
//
// The package depends on nothing outside the standard library. That is a
// requirement, not an accident: its consumers ship as single small static
// binaries, and importing this must not change either one's dependency
// graph.
package whippetrpc

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

// DefaultTimeout is the request timeout used when Config.HTTP is nil.
const DefaultTimeout = 30 * time.Second

// DefaultID is the JSON-RPC request id used when Config.ID is empty.
const DefaultID = "whippet"

// NodeRejection is an error the node itself returned: the RPC call reached
// whippetd, and whippetd said no. It is distinct from a transport failure,
// which means the call never got an answer at all.
//
// The distinction matters at the HTTP layer. Telling a user their perfectly
// valid transaction was rejected, when in fact the node was unreachable,
// sends them off debugging a transaction that was never seen. Callers use
// errors.As to tell the two apart -- never a substring match on the message,
// which silently misclassifies as soon as wording changes.
type NodeRejection struct {
	Method  string
	Code    int
	Message string
}

func (e *NodeRejection) Error() string {
	return fmt.Sprintf("rpc %s: %s (code %d)", e.Method, e.Message, e.Code)
}

// Config describes how to reach a node. Everything that differs between
// consumers is set here, once, at construction.
type Config struct {
	// URL is the full endpoint, e.g. "http://127.0.0.1:33665/". When empty
	// it is built from Host and Port.
	URL string
	// Host and Port build URL when URL is empty.
	Host string
	Port int

	// User and Pass are HTTP basic auth credentials. Both empty means the
	// request is sent unauthenticated.
	User string
	Pass string
	// CookieFile, when set, is read for "user:pass" and overrides User/Pass.
	CookieFile string

	// ID is the JSON-RPC request id. Defaults to DefaultID.
	ID string
	// DebugEnv names an environment variable which, when set to a non-empty
	// value, dumps each request and response to stderr. Empty disables it.
	DebugEnv string

	// HTTP is the transport. Defaults to a client with DefaultTimeout.
	HTTP *http.Client
}

// Client is a JSON-RPC client for whippetd. It is safe for concurrent use.
type Client struct {
	url        string
	user, pass string
	id         string
	debugEnv   string
	http       *http.Client
}

// New builds a Client from cfg. It returns an error only if a cookie file was
// requested and could not be read or parsed.
func New(cfg Config) (*Client, error) {
	user, pass := cfg.User, cfg.Pass
	if cfg.CookieFile != "" {
		data, err := os.ReadFile(cfg.CookieFile)
		if err != nil {
			return nil, fmt.Errorf("reading rpc cookie file: %w", err)
		}
		parts := strings.SplitN(strings.TrimSpace(string(data)), ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("malformed rpc cookie file %s", cfg.CookieFile)
		}
		user, pass = parts[0], parts[1]
	}

	url := cfg.URL
	if url == "" {
		url = fmt.Sprintf("http://%s:%d/", cfg.Host, cfg.Port)
	}

	id := cfg.ID
	if id == "" {
		id = DefaultID
	}

	httpClient := cfg.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultTimeout}
	}

	return &Client{
		url:      url,
		user:     user,
		pass:     pass,
		id:       id,
		debugEnv: cfg.DebugEnv,
		http:     httpClient,
	}, nil
}

// URL returns the endpoint the client posts to.
func (c *Client) URL() string { return c.url }

// User returns the resolved basic-auth user (after any cookie file was read).
func (c *Client) User() string { return c.user }

// Pass returns the resolved basic-auth password.
func (c *Client) Pass() string { return c.pass }

// HTTPClient returns the transport in use.
func (c *Client) HTTPClient() *http.Client { return c.http }

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

// Call performs a single JSON-RPC call. On success it unmarshals the result
// into out, which may be nil to discard it. If the node answered with an
// error object, the returned error is a *NodeRejection; any other error means
// the call did not produce a usable answer.
func (c *Client) Call(method string, params []interface{}, out interface{}) error {
	body, err := json.Marshal(rpcRequest{JSONRPC: "1.0", ID: c.id, Method: method, Params: params})
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
	if c.debugEnv != "" && os.Getenv(c.debugEnv) != "" {
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
		return &NodeRejection{Method: method, Code: rr.Error.Code, Message: rr.Error.Message}
	}
	if out != nil {
		return json.Unmarshal(rr.Result, out)
	}
	return nil
}
