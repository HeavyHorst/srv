package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"srv/internal/config"
	"srv/internal/model"
)

func TestListWorkloadKindsAndOwnership(t *testing.T) {
	ctx := context.Background()
	st := newServiceTestStore(t)
	a := &App{store: st, cfg: config.Config{AdminUsers: []string{"admin"}}}
	for _, spec := range []struct{ name, kind, owner string }{
		{"vm-one", "", "alice"}, {"orb-one", model.KindOrb, "alice"}, {"orb-other", model.KindOrb, "bob"},
	} {
		inst := serviceTestInstance(spec.name, model.StateStopped, time.Now().UTC())
		inst.Kind, inst.CreatedByUser = spec.kind, spec.owner
		if err := st.CreateInstance(ctx, inst); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		args  []string
		owner string
		want  string
	}{
		{[]string{"list"}, "alice", "vm-one:vm"},
		{[]string{"orb", "list"}, "alice", "orb-one:orb"},
		{[]string{"list", "--all"}, "alice", "vm-one:vm,orb-one:orb"},
		{[]string{"orb", "list"}, "admin", "orb-one:orb,orb-other:orb"},
	} {
		result, err := a.dispatch(ctx, model.Actor{UserLogin: tc.owner}, commandRequest{args: tc.args, format: outputFormatJSON})
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Instances []instanceSummaryJSON `json:"instances"`
		}
		if err := json.Unmarshal([]byte(result.stdout), &payload); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, inst := range payload.Instances {
			got = append(got, inst.Name+":"+inst.Kind)
		}
		if strings.Join(got, ",") != tc.want {
			t.Fatalf("%v %s: got %v, want %s", tc.args, tc.owner, got, tc.want)
		}
	}
	result, err := a.dispatch(ctx, model.Actor{UserLogin: "alice"}, commandRequest{args: []string{"list"}})
	if err != nil || strings.Contains(result.stdout, "orb-one") || !strings.Contains(result.stdout, "vm-one") {
		t.Fatalf("text listing = %s, err = %v", result.stdout, err)
	}
}
