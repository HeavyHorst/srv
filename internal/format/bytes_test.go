package format

import "strings"
import "testing"

func TestBarWide(t *testing.T) {
	tests := []struct {
		width int
		used  int64
		total int64
		want  string
	}{
		{10, 0, 100, "[░░░░░░░░░░]"},
		{10, 50, 100, "[█████░░░░░]"},
		{10, 100, 100, "[██████████]"},
		{10, 0, 0, "[░░░░░░░░░░]"},
		{20, 50, 100, "[██████████░░░░░░░░░░]"},
	}
	for _, tt := range tests {
		got := BarWide(tt.width, tt.used, tt.total)
		if !strings.Contains(got, tt.want) {
			t.Errorf("BarWide(%d, %d, %d) = %q, want containing %q", tt.width, tt.used, tt.total, got, tt.want)
		}
	}
}
