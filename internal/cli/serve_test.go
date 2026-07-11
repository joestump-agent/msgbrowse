package cli

import (
	"testing"
)

func TestResolveListenAddr(t *testing.T) {
	const configured = "127.0.0.1:8787"

	cases := []struct {
		name  string
		flags map[string]string
		want  string
		isErr bool
	}{
		{"defaults to configured", nil, "127.0.0.1:8787", false},
		{"port override", map[string]string{"port": "8888"}, "127.0.0.1:8888", false},
		{"host override", map[string]string{"host": "0.0.0.0"}, "0.0.0.0:8787", false},
		{"host+port override", map[string]string{"host": "0.0.0.0", "port": "9000"}, "0.0.0.0:9000", false},
		{"listen-addr wins", map[string]string{"listen-addr": "192.168.1.5:80", "port": "9000"}, "192.168.1.5:80", false},
		{"invalid port", map[string]string{"port": "70000"}, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := newServeCommand()
			for k, v := range c.flags {
				if err := cmd.Flags().Set(k, v); err != nil {
					t.Fatalf("set %s=%s: %v", k, v, err)
				}
			}
			got, err := resolveListenAddr(cmd, configured)
			if c.isErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("resolveListenAddr = %q, want %q", got, c.want)
			}
		})
	}
}
