package driver

import (
	"slices"
	"testing"
)

// local-file: patterns, or "none" alone, as the chart's ValidatingAdmissionPolicy accepts them.
func TestParseMountSettingsLocalFile(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want []string // nil: refused
	}{
		{"*.tmp  .~lock*", []string{"*.tmp", ".~lock*"}},
		{"none", []string{"none"}},
		{" none ", []string{"none"}},
		{"nonesuch *.tmp", []string{"nonesuch", "*.tmp"}},
		{"none *.tmp", nil},
		{"*.tmp none", nil},
		{"none none", nil},
	} {
		ms, err := parseMountSettings(map[string]string{AnnotationUID: "1000", AnnotationLocalFile: tc.raw})
		switch {
		case tc.want == nil && err == nil:
			t.Errorf("local-file %q accepted as %q", tc.raw, ms.Options.LocalFile)
		case tc.want != nil && err != nil:
			t.Errorf("local-file %q: %v", tc.raw, err)
		case tc.want != nil && !slices.Equal(ms.Options.LocalFile, tc.want):
			t.Errorf("local-file %q = %q, want %q", tc.raw, ms.Options.LocalFile, tc.want)
		}
	}
}

// warmup-depth: an integer from 0 (every level), as the chart's ValidatingAdmissionPolicy accepts it.
func TestParseMountSettingsWarmupDepth(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int // -1: refused
	}{
		{"0", 0},
		{"3", 3},
		{"007", 7},
		{"2147483647", 2147483647},
		{"2147483648", -1},
		{"-1", -1},
		{"+2", -1},
		{"two", -1},
		{"", -1},
	} {
		ms, err := parseMountSettings(map[string]string{AnnotationUID: "1000", AnnotationWarmupDepth: tc.raw})
		switch {
		case tc.want < 0 && err == nil:
			t.Errorf("warmup-depth %q accepted as %d", tc.raw, ms.Options.WarmupDepth)
		case tc.want >= 0 && err != nil:
			t.Errorf("warmup-depth %q: %v", tc.raw, err)
		case tc.want >= 0 && ms.Options.WarmupDepth != tc.want:
			t.Errorf("warmup-depth %q = %d, want %d", tc.raw, ms.Options.WarmupDepth, tc.want)
		}
	}
}
