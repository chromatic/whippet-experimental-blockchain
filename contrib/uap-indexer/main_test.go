package main

import (
	"flag"
	"io"
	"testing"
	"time"
)

// newTestFlagSet returns a FlagSet that reports errors instead of calling
// os.Exit, and does not print usage into the test log.
func newTestFlagSet(t *testing.T) *flag.FlagSet {
	t.Helper()
	fs := flag.NewFlagSet("uap-indexer", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// TestConfigPrecedence pins the documented order: an explicit flag beats an
// environment variable, which beats the built-in default.
//
// The credential rows are the ones with teeth. The deployment story is that
// RPC credentials arrive through systemd's EnvironmentFile rather than argv
// (argv is world-readable via /proc/<pid>/cmdline), so "env alone is enough
// to configure them" has to keep working -- while an operator debugging by
// hand with a flag still has to win over whatever the env file says.
func TestConfigPrecedence(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		args []string
		want func(*config) (got, want interface{})
	}{
		{
			name: "default when neither flag nor env is set",
			want: func(c *config) (interface{}, interface{}) { return c.rpcHost, "127.0.0.1" },
		},
		{
			name: "env alone configures the RPC user",
			env:  map[string]string{"UAP_RPC_USER": "from-env"},
			want: func(c *config) (interface{}, interface{}) { return c.rpcUser, "from-env" },
		},
		{
			name: "env alone configures the RPC password",
			env:  map[string]string{"UAP_RPC_PASSWORD": "from-env"},
			want: func(c *config) (interface{}, interface{}) { return c.rpcPass, "from-env" },
		},
		{
			name: "explicit flag beats env for the RPC user",
			env:  map[string]string{"UAP_RPC_USER": "from-env"},
			args: []string{"-rpcuser=from-flag"},
			want: func(c *config) (interface{}, interface{}) { return c.rpcUser, "from-flag" },
		},
		{
			name: "explicit flag beats env for the RPC password",
			env:  map[string]string{"UAP_RPC_PASSWORD": "from-env"},
			args: []string{"-rpcpassword=from-flag"},
			want: func(c *config) (interface{}, interface{}) { return c.rpcPass, "from-flag" },
		},
		{
			name: "explicit flag beats env for the cookie file",
			env:  map[string]string{"UAP_RPC_COOKIEFILE": "/env/.cookie"},
			args: []string{"-rpccookiefile=/flag/.cookie"},
			want: func(c *config) (interface{}, interface{}) { return c.rpcCookie, "/flag/.cookie" },
		},
		{
			name: "env alone configures the listen address",
			env:  map[string]string{"UAP_LISTEN": "127.0.0.1:9999"},
			want: func(c *config) (interface{}, interface{}) { return c.listen, "127.0.0.1:9999" },
		},
		{
			name: "explicit flag beats env for the listen address",
			env:  map[string]string{"UAP_LISTEN": "127.0.0.1:9999"},
			args: []string{"-listen=127.0.0.1:1234"},
			want: func(c *config) (interface{}, interface{}) { return c.listen, "127.0.0.1:1234" },
		},
		{
			name: "env alone configures the state file",
			env:  map[string]string{"UAP_STATEFILE": "/var/lib/uap-indexer/state.sqlite"},
			want: func(c *config) (interface{}, interface{}) {
				return c.stateFile, "/var/lib/uap-indexer/state.sqlite"
			},
		},
		{
			name: "env alone configures the RPC port",
			env:  map[string]string{"UAP_RPC_PORT": "18332"},
			want: func(c *config) (interface{}, interface{}) { return c.rpcPort, 18332 },
		},
		{
			name: "explicit flag beats env for the RPC port",
			env:  map[string]string{"UAP_RPC_PORT": "18332"},
			args: []string{"-rpcport=44444"},
			want: func(c *config) (interface{}, interface{}) { return c.rpcPort, 44444 },
		},
		{
			// A typo in the env file must not take the service down; the
			// built-in default stands and the flag remains available.
			name: "unparseable int env falls back to the default",
			env:  map[string]string{"UAP_RPC_PORT": "banana"},
			want: func(c *config) (interface{}, interface{}) { return c.rpcPort, 33665 },
		},
		{
			name: "env alone enables trustproxy",
			env:  map[string]string{"UAP_TRUSTPROXY": "true"},
			want: func(c *config) (interface{}, interface{}) { return c.trustProxy, true },
		},
		{
			name: "env accepts 1 for trustproxy",
			env:  map[string]string{"UAP_TRUSTPROXY": "1"},
			want: func(c *config) (interface{}, interface{}) { return c.trustProxy, true },
		},
		{
			// The flag is the operator's explicit statement; an env file left
			// over from another deployment must not re-enable header trust.
			name: "explicit -trustproxy=false beats env",
			env:  map[string]string{"UAP_TRUSTPROXY": "true"},
			args: []string{"-trustproxy=false"},
			want: func(c *config) (interface{}, interface{}) { return c.trustProxy, false },
		},
		{
			name: "explicit -trustproxy beats an env that disables it",
			env:  map[string]string{"UAP_TRUSTPROXY": "false"},
			args: []string{"-trustproxy"},
			want: func(c *config) (interface{}, interface{}) { return c.trustProxy, true },
		},
		{
			// "yes" is not a value ParseBool accepts. Failing closed is the
			// only safe reading: silently turning header trust on because a
			// value could not be parsed is exactly the accident this flag
			// is meant to prevent.
			name: "unparseable trustproxy env does not enable it",
			env:  map[string]string{"UAP_TRUSTPROXY": "yes"},
			want: func(c *config) (interface{}, interface{}) { return c.trustProxy, false },
		},
		{
			name: "trustproxy defaults to off",
			want: func(c *config) (interface{}, interface{}) { return c.trustProxy, false },
		},
		{
			// No env equivalent by design -- these are tuning, not secrets.
			name: "durations still parse from flags",
			args: []string{"-pollinterval=2s"},
			want: func(c *config) (interface{}, interface{}) { return c.pollInterval, 2 * time.Second },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			cfg, err := parseConfig(newTestFlagSet(t), tt.args)
			if err != nil {
				t.Fatalf("parseConfig(%v) returned an error: %v", tt.args, err)
			}
			got, want := tt.want(cfg)
			if got != want {
				t.Errorf("got %#v, want %#v (env %v, args %v)", got, want, tt.env, tt.args)
			}
		})
	}
}

// Credentials must never be *required* on the command line, since anything
// in argv is readable by any local user via /proc. This asserts the
// environment path alone is sufficient to fully configure RPC auth.
func TestCredentialsConfigurableWithoutArgv(t *testing.T) {
	t.Setenv("UAP_RPC_USER", "placeholder-user")
	t.Setenv("UAP_RPC_PASSWORD", "placeholder-password")

	cfg, err := parseConfig(newTestFlagSet(t), nil)
	if err != nil {
		t.Fatalf("parseConfig returned an error: %v", err)
	}
	if cfg.rpcUser != "placeholder-user" || cfg.rpcPass != "placeholder-password" {
		t.Fatalf("credentials not taken from the environment: user=%q pass set=%v",
			cfg.rpcUser, cfg.rpcPass != "")
	}
}

func TestEnvOrHelpers(t *testing.T) {
	t.Run("envOr returns the default when unset", func(t *testing.T) {
		if got := envOr("UAP_TEST_DEFINITELY_UNSET", "fallback"); got != "fallback" {
			t.Errorf("envOr = %q, want %q", got, "fallback")
		}
	})
	t.Run("envOr returns an empty value when explicitly set empty", func(t *testing.T) {
		// Distinguishing "set to empty" from "unset" is what makes it
		// possible to clear a value inherited from an env file.
		t.Setenv("UAP_TEST_EMPTY", "")
		if got := envOr("UAP_TEST_EMPTY", "fallback"); got != "" {
			t.Errorf("envOr = %q, want an empty string", got)
		}
	})
	t.Run("envOrInt rejects garbage", func(t *testing.T) {
		t.Setenv("UAP_TEST_INT", "12x")
		if got := envOrInt("UAP_TEST_INT", 7); got != 7 {
			t.Errorf("envOrInt = %d, want 7", got)
		}
	})
	t.Run("envOrBool rejects garbage without flipping to true", func(t *testing.T) {
		for _, v := range []string{"yes", "on", "", "TRUE-ish", "2x"} {
			t.Setenv("UAP_TEST_BOOL", v)
			if envOrBool("UAP_TEST_BOOL", false) {
				t.Errorf("envOrBool(%q) = true, want false: an unparseable value must not enable a security-sensitive flag", v)
			}
		}
	})
}
