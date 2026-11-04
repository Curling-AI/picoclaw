package messageutil

import "testing"

func TestSourceContentDigestPreservesTheReceiptCanonicalization(t *testing.T) {
	at := "2026-11-02T08:12:00-03:00"
	utc := "2026-11-02T11:12:00Z"
	digest := SourceContentDigest(at, "raw report.pdf")
	if digest != SourceContentDigest(utc, "raw report.pdf") || !ValidSourceContentDigest(digest) ||
		digest == SourceContentDigest(utc, "projected artifact:version.pdf") ||
		ValidSourceContentDigest(ContentDigest(utc, "raw report.pdf")) || ValidSourceContentDigest(digest+"00") {
		t.Fatal("source digest changed its canonical time, content or private version domain")
	}
}
