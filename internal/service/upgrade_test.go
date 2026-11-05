package service

import (
	"context"
	"testing"
	"time"

	"srv/internal/model"
)

func TestUpgradeCommandAuthorizationAndArguments(t *testing.T) {
	st := newServiceTestStore(t)
	a := &App{store: st}
	inst := serviceTestInstance("alpha", model.StateStopped, time.Now())
	if err := st.CreateInstance(context.Background(), inst); err != nil {
		t.Fatal(err)
	}
	// No provisioner: a rejected request must not reach filesystem operations.
	for _, args := range [][]string{{"upgrade"}, {"upgrade", "alpha", "--force"}, {"upgrade", "alpha"}, {"upgrade", "alpha", "--rollback"}, {"upgrade", "alpha", "--finalize"}, {"upgrade", "alpha", "--rollback", "--finalize"}} {
		result, err := a.cmdUpgrade(context.Background(), model.Actor{UserLogin: "bob@example.com"}, args, outputFormatJSON)
		if err == nil || result.exitCode == 0 {
			t.Fatalf("accepted unauthorized/malformed command %v", args)
		}
	}
}
