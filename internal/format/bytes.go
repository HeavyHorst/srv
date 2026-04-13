package format

import "fmt"

func BarWide(width int, used, total int64) string {
	if width <= 0 {
		width = 40
	}
	if total <= 0 {
		return fmt.Sprintf("[%s]", repeatBar(width, '░'))
	}
	pct := float64(used) / float64(total)
	if pct < 0 {
		pct = 0
	}
	if pct > 1 {
		pct = 1
	}
	filled := int(pct * float64(width))
	if pct > 0 && filled == 0 {
		filled = 1
	}
	return fmt.Sprintf("[%s%s]", repeatBar(filled, '█'), repeatBar(width-filled, '░'))
}

func repeatBar(n int, r rune) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = r
	}
	return string(b)
}

const (
	Byte = int64(1)
	KiB  = 1024 * Byte
	MiB  = 1024 * KiB
	GiB  = 1024 * MiB
	TiB  = 1024 * GiB
)

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
