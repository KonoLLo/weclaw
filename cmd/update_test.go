package cmd

import "testing"

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
		ok   bool
	}{
		{"1.2.9", "1.2.9", 0, true},
		{"v1.2.9", "1.2.9", 0, true},
		{"1.2.9", "v1.3.0", -1, true},
		{"1.3.0", "1.2.10", 1, true},
		{"1.2", "1.2.1", -1, true},
		{"dev", "1.3.0", 0, false},
		{"1.3.0", "1.3.0-rc1", 0, false},
	}
	for _, c := range cases {
		got, ok := compareVersions(c.a, c.b)
		if got != c.want || ok != c.ok {
			t.Errorf("compareVersions(%q,%q) = (%d,%v), want (%d,%v)", c.a, c.b, got, ok, c.want, c.ok)
		}
	}
}
