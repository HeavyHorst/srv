package config

import "testing"

func TestProviderConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, env, cli string
		fail           bool
	}{
		{"environment", `{"n-provider":"amp"}`, "", false},
		{"cli override", `{"n-provider":"other"}`, `{"n-provider":"amp"}`, false},
		{"human impersonation", `{"n-provider":"alice@example.com"}`, "", true},
		{"invalid JSON", `no`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"srv.test"}
			if tc.cli != "" {
				args = append(args, "--provider-nodes", tc.cli)
			}
			resetFlagsForTest(t, args)
			t.Setenv("SRV_PROVIDER_NODES", tc.env)
			cfg, err := Load()
			if (err != nil) != tc.fail {
				t.Fatalf("Load: %v", err)
			}
			if tc.fail {
				return
			}
			if cfg.ProviderNodes["n-provider"] != "amp" {
				t.Fatalf("mapping: %v", cfg.ProviderNodes)
			}
			cfg.AdminUsers = []string{"service:amp"}
			if cfg.Validate() == nil {
				t.Fatal("service admin accepted")
			}
			cfg.AdminUsers = nil
			cfg.TrustedFrontends = []string{"n-provider"}
			if cfg.Validate() == nil {
				t.Fatal("provider as trusted frontend accepted")
			}
		})
	}
}
