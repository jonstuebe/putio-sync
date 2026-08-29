package progress

import (
	"testing"
	"time"
)

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1 << 20, "1.0 MB"},
		{1468006, "1.4 MB"},
		{933232640, "890 MB"},
		{2254857830, "2.1 GB"},
		{7838315479, "7.3 GB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.n); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestHumanBytePairSharesAUnit(t *testing.T) {
	// The two numbers are only comparable at a glance if they use the same
	// unit, even when the first one would scale differently on its own.
	if got, want := humanBytePair(4509715660, 7838315479), "4.2/7.3 GB"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := humanBytePair(1024, 1<<30), "0.0/1.0 GB"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestHumanRate(t *testing.T) {
	if got, want := humanRate(11953766), "11.4 MB/s"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// No rate yet is not a rate of zero.
	if got, want := humanRate(0), "--"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{26 * time.Second, "26s"},
		{time.Minute, "1m00s"},
		{3*time.Minute + 12*time.Second, "3m12s"},
		{time.Hour + 4*time.Minute, "1h04m"},
		{25 * time.Hour, "25h00m"},
	}
	for _, c := range cases {
		if got := humanDuration(c.d); got != c.want {
			t.Errorf("humanDuration(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestFormatETA(t *testing.T) {
	cases := []struct {
		name      string
		remaining int64
		rate      int64
		want      string
	}{
		{"warming up", 1 << 30, 0, "--"},
		{"nothing left", 0, 1 << 20, "--"},
		{"ordinary", 100 << 20, 10 << 20, "10s"},
		// An estimate this far out is not information.
		{"beyond a day", 1 << 40, 1 << 10, "--"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := formatETA(c.remaining, c.rate); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestTruncateMiddleKeepsTheExtension(t *testing.T) {
	const name = "Big.Buck.Bunny.2160p.mkv"
	cases := []struct {
		max  int
		want string
	}{
		{40, name},
		{24, name},
		{18, "Big.Buck…2160p.mkv"},
		{10, "Big.…p.mkv"},
		{1, "…"},
		{0, ""},
	}
	for _, c := range cases {
		got := truncateMiddle(name, c.max)
		if got != c.want {
			t.Errorf("truncateMiddle(%q, %d) = %q, want %q", name, c.max, got, c.want)
		}
		if len([]rune(got)) > c.max {
			t.Errorf("truncateMiddle(%q, %d) = %q, which is %d characters", name, c.max, got, len([]rune(got)))
		}
	}
}

func TestTruncateMiddleCountsRunesNotBytes(t *testing.T) {
	const name = "Grüße.aus.München.mkv"
	got := truncateMiddle(name, 12)
	if n := len([]rune(got)); n != 12 {
		t.Errorf("truncateMiddle(%q, 12) = %q, which is %d characters", name, got, n)
	}
}

func TestPadding(t *testing.T) {
	if got, want := padRight("ab", 5), "ab   "; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := padLeft("ab", 5), "   ab"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// Padding never truncates.
	if got, want := padRight("abcdef", 3), "abcdef"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := padRight("Grüße", 7), "Grüße  "; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := spaces(100), 100; len(got) != want {
		t.Errorf("spaces(100) is %d characters, want %d", len(got), want)
	}
}
