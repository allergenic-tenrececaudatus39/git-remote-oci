package helper

import "testing"

// TestParseBlobLimit covers the sizes git actually sends.
//
// The previous implementation handed the raw string to strconv.ParseInt, which
// rejects every suffixed form, and then discarded the error - so
// --filter=blob:limit=100k silently filtered nothing.
func TestParseBlobLimit(t *testing.T) {
	ok := []struct {
		spec string
		want int64
	}{
		{"0", 0},
		{"512", 512},
		{"100k", 100 << 10},
		{"100K", 100 << 10},
		{"1m", 1 << 20},
		{"1M", 1 << 20},
		{"2g", 2 << 30},
		{"2G", 2 << 30},
		{" 64k ", 64 << 10}, // surrounding whitespace is trimmed
	}
	for _, tt := range ok {
		got, err := parseBlobLimit(tt.spec)
		if err != nil {
			t.Errorf("parseBlobLimit(%q) returned an error: %v", tt.spec, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseBlobLimit(%q) = %d, want %d", tt.spec, got, tt.want)
		}
	}

	// A limit that cannot be understood must be reported. Ignoring it means
	// transferring blobs the user asked to skip while claiming the filter was
	// applied.
	for _, spec := range []string{"", "   ", "abc", "10x", "-5", "1.5m", "99999999999999999999g"} {
		if got, err := parseBlobLimit(spec); err == nil {
			t.Errorf("parseBlobLimit(%q) = %d, want an error", spec, got)
		}
	}
}
