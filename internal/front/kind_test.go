package front_test

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"strings"
	"testing"

	"srv/internal/front"
	"srv/internal/model"
)

func TestOrbBootstrapRoutesAcrossKindsAndPreservesStdin(t *testing.T) {
	called := false
	r := router(func(h front.Host, args []string, in io.Reader, out io.Writer) error {
		if reflect.DeepEqual(args, []string{"--json", "list", "--all"}) {
			if h.Name == "b" {
				io.WriteString(out, `{"instances":[{"name":"work","kind":"orb"}]}`)
			} else {
				io.WriteString(out, `{"instances":[]}`)
			}
			return nil
		}
		if h.Name != "b" || !reflect.DeepEqual(args, []string{"orb", "bootstrap", "work"}) {
			t.Fatalf("wrong route: %s %v", h.Name, args)
		}
		if in == nil {
			t.Fatal("missing stdin")
		}
		body, err := io.ReadAll(in)
		if err != nil || string(body) != "secret-stdin" {
			t.Fatalf("stdin = %q, %v", body, err)
		}
		called = true
		return nil
	})
	var out bytes.Buffer
	if err := r.Run(context.Background(), model.Actor{}, []string{"orb", "bootstrap", "work"}, strings.NewReader("secret-stdin"), &out, &out); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("bootstrap not called")
	}
}

func TestOrbListAggregatesWithoutChangingCommand(t *testing.T) {
	r := router(func(h front.Host, args []string, in io.Reader, out io.Writer) error {
		if !reflect.DeepEqual(args, []string{"--json", "orb", "list"}) {
			t.Fatalf("args = %v", args)
		}
		io.WriteString(out, `{"instances":[{"name":"work","kind":"orb"}]}`)
		return nil
	})
	out, _, err := run(r, "--json", "orb", "list")
	if err != nil || !strings.Contains(out, `"kind":"orb"`) || !strings.Contains(out, `"hosts"`) {
		t.Fatalf("%s %v", out, err)
	}
}

func TestOrbNewUsesPlacementAndKeepsKindCommand(t *testing.T) {
	created := false
	r := router(func(h front.Host, args []string, in io.Reader, out io.Writer) error {
		if reflect.DeepEqual(args, []string{"--json", "placement", "work", "--ram", "2G"}) {
			io.WriteString(out, `{"eligible":true,"memory_left_mib":100,"disk_left_bytes":100}`)
			return nil
		}
		if h.Name != "a" || !reflect.DeepEqual(args, []string{"orb", "new", "work", "--ram", "2G"}) {
			t.Fatalf("create route: %s %v", h.Name, args)
		}
		created = true
		return nil
	})
	if _, _, err := run(r, "orb", "new", "work", "--ram", "2G"); err != nil || !created {
		t.Fatalf("created = %v, err = %v", created, err)
	}
}
