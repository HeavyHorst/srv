package front_test

import (
	"io"
	"strings"
	"testing"

	"srv/internal/front"
)

func TestHostLocalTemplatesRequireExplicitHost(t *testing.T) {
	for _, args := range [][]string{
		{"new", "vm", "--template", "id"},
		{"new", "vm", "--template=id"},
		{"project-template", "create", "vm", "id"},
		{"project-template", "delete", "id"},
	} {
		calls := 0
		r := router(func(h front.Host, got []string, _ io.Reader, _ io.Writer) error {
			calls++
			if h.Name != "a" || strings.Join(got, " ") != strings.Join(args, " ") {
				t.Fatalf("wrong routing: %s %v", h.Name, got)
			}
			return nil
		})
		if _, _, err := run(r, args...); err == nil || !strings.Contains(err.Error(), "requires --host") || calls != 0 {
			t.Fatalf("automatic routing: %v calls=%d", err, calls)
		}
		if _, _, err := run(r, append(append([]string{}, args...), "--host", "a")...); err != nil || calls != 1 {
			t.Fatalf("pinned routing: %v calls=%d", err, calls)
		}
	}
}
