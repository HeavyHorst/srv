package format

import "strings"
import "testing"

func TestBar(t *testing.T) {
	tests := []struct {
		used  int64
		total int64
		want  string
	}{
		{0, 100, "░░░░░░░░   0%"},
		{1, 100, "█░░░░░░░   1%"},
		{50, 100, "████░░░░  50%"},
		{99, 100, "███████░  99%"},
		{100, 100, "████████ 100%"},
		{150, 100, "████████ 100%"},
		{0, 0, "░░░░░░░░   0%"},
		{5, 0, "░░░░░░░░   0%"},
	}
	for _, tt := range tests {
		got := Bar(tt.used, tt.total)
		if !strings.Contains(got, tt.want) {
			t.Errorf("Bar(%d, %d) = %q, want containing %q", tt.used, tt.total, got, tt.want)
		}
	}
}
