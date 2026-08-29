package progress

import (
	"fmt"
	"time"
)

// maxETA is the point past which an estimate stops being information. A
// transfer that claims to have four days left is really saying "no idea".
const maxETA = 24 * time.Hour

var byteUnits = []string{"B", "KB", "MB", "GB", "TB", "PB"}

// scaleBytes returns the divisor and unit to render n in.
func scaleBytes(n int64) (float64, string) {
	div, unit := float64(1), byteUnits[0]
	v := float64(n)
	for i := 1; i < len(byteUnits) && v >= 1024; i++ {
		v /= 1024
		div *= 1024
		unit = byteUnits[i]
	}
	return div, unit
}

func formatScaled(n int64, div float64) string {
	v := float64(n) / div
	if v < 100 && div > 1 {
		return fmt.Sprintf("%.1f", v)
	}
	return fmt.Sprintf("%.0f", v)
}

// humanBytes renders a byte count, e.g. "4.2 GB".
func humanBytes(n int64) string {
	div, unit := scaleBytes(n)
	return formatScaled(n, div) + " " + unit
}

// humanBytePair renders "done/total", scaled to the same unit so the two
// numbers can be compared at a glance, e.g. "4.2/7.3 GB".
func humanBytePair(done, total int64) string {
	div, unit := scaleBytes(total)
	return formatScaled(done, div) + "/" + formatScaled(total, div) + " " + unit
}

// humanRate renders a speed, e.g. "11.4 MB/s".
func humanRate(bytesPerSecond int64) string {
	if bytesPerSecond <= 0 {
		return "--"
	}
	return humanBytes(bytesPerSecond) + "/s"
}

// humanDuration renders a duration compactly: "26s", "3m12s", "1h04m".
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// formatETA estimates how long the remaining bytes will take. It reports "--"
// rather than a number it does not believe: before the rate counter has
// anything to average, and beyond the point where the estimate is meaningless.
func formatETA(remaining, bytesPerSecond int64) string {
	if bytesPerSecond <= 0 || remaining <= 0 {
		return "--"
	}
	d := time.Duration(remaining/bytesPerSecond) * time.Second
	if d > maxETA {
		return "--"
	}
	return humanDuration(d)
}

// truncateMiddle shortens s to max characters, cutting from the middle so that
// both the beginning of the name and the extension survive.
func truncateMiddle(s string, max int) string {
	r := []rune(s)
	if max <= 0 {
		return ""
	}
	if len(r) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	// The tail gets the odd character: the extension is worth more than one
	// extra letter of the stem.
	keep := max - 1
	head := keep / 2
	tail := keep - head
	return string(r[:head]) + "…" + string(r[len(r)-tail:])
}

// padRight pads s with spaces to width characters. It counts runes, so a name
// with non-ASCII characters still lines up.
func padRight(s string, width int) string {
	n := width - len([]rune(s))
	if n <= 0 {
		return s
	}
	return s + spaces(n)
}

// padLeft right-aligns s in width characters.
func padLeft(s string, width int) string {
	n := width - len([]rune(s))
	if n <= 0 {
		return s
	}
	return spaces(n) + s
}

func spaces(n int) string {
	const s = "                                                                "
	if n <= len(s) {
		return s[:n]
	}
	out := make([]byte, n)
	for i := range out {
		out[i] = ' '
	}
	return string(out)
}
