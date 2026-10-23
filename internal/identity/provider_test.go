package identity

import (
	"testing"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
)

func TestProviderResolution(t *testing.T) {
	p := Providers{"n-provider": "amp"}
	for _, tc := range []struct {
		name, node, login, want string
		tagged                  bool
	}{
		{"tagged provider", "n-provider", "admin@example.com", "service:amp", true},
		{"untagged provider", "n-provider", "admin@example.com", "service:amp", false},
		{"tagged unknown", "n-other", "admin@example.com", "", true},
		{"unknown without profile", "n-other", "", "", false},
		{"human unchanged", "n-other", "alice@example.com", "alice@example.com", false},
		{"reserved human login", "n-other", "SERVICE:amp", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			who := &apitype.WhoIsResponse{Node: &tailcfg.Node{StableID: tailcfg.StableNodeID(tc.node)}, UserProfile: &tailcfg.UserProfile{LoginName: tc.login}}
			if tc.tagged {
				who.Node.Tags = []string{"tag:provider"}
			}
			a, err := p.Resolve(who, "peer", "admin@example.com")
			if (err != nil) != (tc.want == "") || a.UserLogin != tc.want {
				t.Fatalf("actor=%+v err=%v", a, err)
			}
		})
	}
	if _, err := p.Resolve(nil, "", ""); err == nil {
		t.Fatal("nil identity accepted")
	}
}

func TestProviderValidation(t *testing.T) {
	for _, p := range []Providers{{"*": "amp"}, {"": "amp"}, {"n": "alice@example.com"}, {"n": "service:amp"}, {"n": "Amp"}} {
		if p.Validate(nil) == nil {
			t.Fatalf("accepted invalid mapping %v", p)
		}
	}
	if (Providers{"n": "amp"}).Validate([]string{"SERVICE:amp"}) == nil {
		t.Fatal("service admin accepted")
	}
}
