package transport

import "testing"

// The stats parser is the source of the headline "X on the wire" number, and it
// once silently mangled rsync's --human-readable output ("85.38M" became 8538
// bytes). These cases lock the raw format in.
func TestParseStat(t *testing.T) {
	lines := []string{
		"Number of files: 14 (reg: 13, dir: 1)",
		"Total file size: 85,379,846 bytes",
		"Total transferred file size: 85,379,846 bytes",
		"Literal data: 10,546 bytes",
		"Matched data: 85,369,300 bytes",
		"Total bytes sent: 5,323",
		"Total bytes received: 214",
	}
	var st Stats
	for _, l := range lines {
		parseStat(l, &st)
	}
	if st.Total != 85379846 {
		t.Errorf("Total = %d, want 85379846", st.Total)
	}
	if st.Literal != 10546 {
		t.Errorf("Literal = %d, want 10546", st.Literal)
	}
	if st.Matched != 85369300 {
		t.Errorf("Matched = %d, want 85369300", st.Matched)
	}
	if st.Sent != 5323 {
		t.Errorf("Sent = %d, want 5323", st.Sent)
	}
	if got := st.DedupPercent(); got < 99.9 {
		t.Errorf("DedupPercent = %f, want ~100", got)
	}
}

func TestParseStatIgnoresUnrelatedLines(t *testing.T) {
	var st Stats
	parseStat("sending incremental file list", &st)
	parseStat("blobs/sha256/abc123", &st)
	if st != (Stats{}) {
		t.Errorf("unrelated lines mutated stats: %+v", st)
	}
}
