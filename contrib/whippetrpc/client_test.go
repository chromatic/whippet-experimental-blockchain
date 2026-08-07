package whippetrpc

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCall covers the four outcomes a call can have: an answer, a refusal
// from the node, an HTTP status that means we never got a JSON-RPC answer at
// all, and a body that is not JSON.
func TestCall(t *testing.T) {
	tests := []struct {
		name string
		// handler stands in for whippetd. httptest is an in-process server;
		// no node is involved.
		handler http.HandlerFunc
		// out is unmarshalled into when the call succeeds.
		wantHeight int64
		wantErr    string
		// wantRejection, when set, is the *NodeRejection errors.As must find.
		wantRejection *NodeRejection
	}{
		{
			name: "success",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(`{"result":4242,"error":null,"id":"test"}`))
			},
			wantHeight: 4242,
		},
		{
			name: "node rejection surfaces as *NodeRejection",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"result":null,"error":{"code":-26,"message":"dust"},"id":"test"}`))
			},
			wantRejection: &NodeRejection{Method: "getblockcount", Code: -26, Message: "dust"},
			wantErr:       "rpc getblockcount: dust (code -26)",
		},
		{
			name: "unexpected HTTP status",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte("nope"))
			},
			wantErr: "rpc getblockcount: unexpected HTTP status 401: nope",
		},
		{
			name: "malformed JSON",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("{not json"))
			},
			wantErr: "rpc getblockcount: decoding response",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			c, err := New(Config{URL: srv.URL + "/", HTTP: srv.Client()})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			var height int64
			err = c.Call("getblockcount", nil, &height)

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Call: unexpected error %v", err)
				}
				if height != tc.wantHeight {
					t.Fatalf("height = %d, want %d", height, tc.wantHeight)
				}
				return
			}

			if err == nil {
				t.Fatalf("Call: expected error %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.wantErr)
			}

			var rejection *NodeRejection
			gotRejection := errors.As(err, &rejection)
			if tc.wantRejection == nil {
				if gotRejection {
					t.Fatalf("errors.As found a *NodeRejection (%v) for a transport-level failure; "+
						"callers would report an unreachable node as the user's invalid request", rejection)
				}
				return
			}
			if !gotRejection {
				t.Fatalf("errors.As did not find a *NodeRejection in %v", err)
			}
			if *rejection != *tc.wantRejection {
				t.Fatalf("rejection = %+v, want %+v", *rejection, *tc.wantRejection)
			}
		})
	}
}

// TestNodeRejectionMessageFormat pins the exact string, byte for byte. The
// faucet used to produce this with a flat fmt.Errorf; if the wording drifts,
// a faucet error message its users have seen changes silently.
func TestNodeRejectionMessageFormat(t *testing.T) {
	e := &NodeRejection{Method: "sendtoaddress", Code: -6, Message: "Insufficient funds"}
	const want = "rpc sendtoaddress: Insufficient funds (code -6)"
	if got := e.Error(); got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

// TestRequestShape checks the parts of the wire format the node cares about:
// JSON-RPC version, the configurable id, and basic auth.
func TestRequestShape(t *testing.T) {
	type captured struct {
		body     rpcRequest
		user     string
		pass     string
		haveAuth bool
	}
	var got captured

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got.body)
		got.user, got.pass, got.haveAuth = r.BasicAuth()
		w.Write([]byte(`{"result":null,"error":null,"id":"x"}`))
	}))
	defer srv.Close()

	c, err := New(Config{URL: srv.URL + "/", User: "u", Pass: "p", ID: "faucet", HTTP: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Call("validateaddress", []interface{}{"DAddr"}, nil); err != nil {
		t.Fatalf("Call: %v", err)
	}

	if got.body.JSONRPC != "1.0" {
		t.Errorf("jsonrpc = %q, want \"1.0\"", got.body.JSONRPC)
	}
	if got.body.ID != "faucet" {
		t.Errorf("id = %q, want \"faucet\"", got.body.ID)
	}
	if got.body.Method != "validateaddress" {
		t.Errorf("method = %q, want \"validateaddress\"", got.body.Method)
	}
	if !got.haveAuth || got.user != "u" || got.pass != "p" {
		t.Errorf("basic auth = (%q, %q, %v), want (\"u\", \"p\", true)", got.user, got.pass, got.haveAuth)
	}
}

// TestNoAuthWhenUserEmpty: an empty user must send no Authorization header at
// all, rather than an empty one, which some setups reject outright.
func TestNoAuthWhenUserEmpty(t *testing.T) {
	var sawAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawAuth = r.Header["Authorization"]
		w.Write([]byte(`{"result":null,"error":null,"id":"x"}`))
	}))
	defer srv.Close()

	c, _ := New(Config{URL: srv.URL + "/", HTTP: srv.Client()})
	if err := c.Call("getblockcount", nil, nil); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if sawAuth {
		t.Fatal("sent an Authorization header with no credentials configured")
	}
}

func TestCookieFile(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "cookie")
	if err := os.WriteFile(good, []byte("__cookie__:secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{Host: "127.0.0.1", Port: 33665, User: "ignored", Pass: "ignored", CookieFile: good})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.User() != "__cookie__" || c.Pass() != "secret" {
		t.Errorf("credentials = (%q, %q), want (\"__cookie__\", \"secret\")", c.User(), c.Pass())
	}
	if c.URL() != "http://127.0.0.1:33665/" {
		t.Errorf("URL = %q", c.URL())
	}

	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("no-colon-here"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{CookieFile: bad}); err == nil {
		t.Error("expected an error for a cookie file with no colon")
	}
	if _, err := New(Config{CookieFile: filepath.Join(dir, "missing")}); err == nil {
		t.Error("expected an error for a missing cookie file")
	}
}
