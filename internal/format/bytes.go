package format

import "fmt"

const barWidth = 8

func Bar(used, total int64) string {
	if total <= 0 {
		return fmt.Sprintf("%s %3d%%", repeatBar(barWidth, '░'), 0)
	}
	pct := float64(used) / float64(total)
	if pct < 0 {
		pct = 0
	}
	if pct > 1 {
		pct = 1
	}
	filled := int(pct * barWidth)
	if pct > 0 && filled == 0 {
		filled = 1
	}
	return fmt.Sprintf("%s%s %3.0f%%", repeatBar(filled, '█'), repeatBar(barWidth-filled, '░'), pct*100)
}

func repeatBar(n int, r rune) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = r
	}
	return string(b)
}

func BinarySize(sizeBytes int64) string {
	if sizeBytes < 1024 {
		return fmt.Sprintf("%d B", sizeBytes)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	size := float64(sizeBytes)
	unit := "B"
	for _, next := range units {
		size /= 1024
		unit = next
		if size < 1024 {
			break
		}
	}
	return fmt.Sprintf("%.1f %s", size, unit)
}
