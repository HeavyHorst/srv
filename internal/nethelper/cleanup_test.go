package nethelper

import (
	"context"
	"errors"
	"testing"
)

func TestCleanupProofRejectsResidualsAndQueryFailures(t *testing.T) {
	q := CleanupRequest{TapDevice: "tap-proof", NetworkCIDR: "172.28.12.0/30"}
	for _, scenario := range []string{"clean", "neighbour", "tap", "route", "host-rule", "guest-rule", "query-failure", "invalid-json", "null", "object", "null-row", "missing-rules"} {
		t.Run(scenario, func(t *testing.T) {
			err := verifyNetworkAbsent(context.Background(), q, func(_ context.Context, name string, args ...string) ([]byte, error) {
				if scenario == "query-failure" {
					return nil, errors.New("permission denied")
				}
				if scenario == "invalid-json" {
					return []byte("invalid"), nil
				}
				if scenario == "null" {
					return []byte("null"), nil
				}
				if scenario == "object" {
					return []byte(`{}`), nil
				}
				if scenario == "null-row" {
					return []byte(`[null]`), nil
				}
				if name == "ip" {
					if scenario == "neighbour" {
						return []byte(`[{"dst":"172.28.12.20/30","dev":"tap-proof-neighbour"}]`), nil
					}
					if scenario == "tap" {
						return []byte(`[{"ifname":"tap-proof"}]`), nil
					}
					if scenario == "route" {
						return []byte(`[{"dst":"172.28.12.0/30"}]`), nil
					}
					return []byte(`[]`), nil
				}
				if name == "nft" {
					if scenario == "missing-rules" {
						return []byte(`{}`), nil
					}
					if scenario == "guest-rule" {
						return []byte(`{"nftables":[{"rule":{"addr":"172.28.12.2"}}]}`), nil
					}
					return []byte(`{"nftables":[]}`), nil
				}
				if scenario == "host-rule" {
					return []byte(`-A FORWARD -s 172.28.12.1/32 -j ACCEPT`), nil
				}
				return []byte(""), nil
			})
			if (err == nil) != (scenario == "clean" || scenario == "neighbour") {
				t.Fatal(err)
			}
		})
	}
}

func TestCleanupProofFindsIPv4InNATAndMappedAddresses(t *testing.T) {
	q := CleanupRequest{TapDevice: "tap-mine000001", NetworkCIDR: "172.28.1.4/30"}
	for _, tc := range []struct {
		rule     string
		residual bool
	}{
		{"--to-destination 172.28.1.6:22", true},
		{"--to-source 172.28.1.4-172.28.1.7", true},
		{"-d ::ffff:172.28.1.6", true},
		{"-d 172.28.1.6/32", true},
		{"-i tap-mine000001", true},
		{"-d 172.28.1.40/30 -i tap-mine000001-other", false},
		{"-d 1172.28.1.6", false},
		{"-d 172.28.1.6.9", false},
	} {
		t.Run(tc.rule, func(t *testing.T) {
			err := verifyNetworkAbsent(context.Background(), q, func(_ context.Context, name string, _ ...string) ([]byte, error) {
				switch name {
				case "ip":
					return []byte(`[]`), nil
				case "nft":
					return []byte(`{"nftables":[]}`), nil
				case "iptables-save":
					return []byte(tc.rule), nil
				default:
					return nil, nil
				}
			})
			if (err != nil) != tc.residual {
				t.Fatalf("residual=%v: %v", tc.residual, err)
			}
		})
	}
}
