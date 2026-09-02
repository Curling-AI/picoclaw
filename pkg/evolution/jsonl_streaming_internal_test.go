package evolution

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"sync/atomic"
	"testing"
	"time"
)

func writeLines(tb testing.TB, lines ...string) string {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "task-records.jsonl")
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		tb.Fatal(err)
	}
	return path
}

func collect(tb testing.TB, path string) []string {
	tb.Helper()
	var got []string
	if err := decodeJSONLLines(path, func(line []byte) error {
		got = append(got, string(line))
		return nil
	}); err != nil {
		tb.Fatalf("decodeJSONLLines: %v", err)
	}
	return got
}

func TestDecodeJSONLLinesKeepsOrderAndSkipsBlanks(t *testing.T) {
	path := writeLines(t, `{"id":"a"}`, "", `{"id":"b"}`, "   ", `{"id":"c"}`)
	got := collect(t, path)
	want := []string{`{"id":"a"}`, `{"id":"b"}`, `{"id":"c"}`}
	if len(got) != len(want) {
		t.Fatalf("read %d lines, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// The reason the whole file used to be buffered: a JSONL append that died
// mid-write leaves a torn last line, and losing the whole store over it would
// be worse than losing that record.
func TestDecodeJSONLLinesToleratesATornLastLine(t *testing.T) {
	path := writeLines(t, `{"id":"a"}`, `{"id":"b"}`, `{"id":"trunc`)
	var seen int
	err := decodeJSONLLines(path, func(line []byte) error {
		seen++
		var probe struct {
			ID string `json:"id"`
		}
		return json.Unmarshal(line, &probe)
	})
	if err != nil {
		t.Fatalf("a torn last line must be tolerated, got %v", err)
	}
	if seen != 3 {
		t.Errorf("decode called %d times, want 3 (the torn one is attempted)", seen)
	}
}

// The flip side, and the one a lookahead could silently break: a torn line in
// the MIDDLE is corruption, not an interrupted append, and must still fail.
func TestDecodeJSONLLinesRejectsATornLineInTheMiddle(t *testing.T) {
	path := writeLines(t, `{"id":"a"}`, `{"id":"trunc`, `{"id":"c"}`)
	err := decodeJSONLLines(path, func(line []byte) error {
		var probe struct {
			ID string `json:"id"`
		}
		return json.Unmarshal(line, &probe)
	})
	if err == nil {
		t.Fatal("a torn line in the middle was accepted; only the last one is forgiven")
	}
}

func TestDecodeJSONLLinesOnAMissingFileIsNotAnError(t *testing.T) {
	if err := decodeJSONLLines(filepath.Join(t.TempDir(), "nope.jsonl"), func([]byte) error {
		t.Fatal("decode called for a file that does not exist")
		return nil
	}); err != nil {
		t.Errorf("missing file returned %v, want nil", err)
	}
}

// peakLiveHeap reports the highest live heap while fn runs. B/op cannot see
// this: it counts every byte allocated, and the line copies are allocated
// either way — what changes is whether they are alive at the same time.
func peakLiveHeap(fn func()) uint64 {
	runtime.GC()
	var peak atomic.Uint64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
		for {
			select {
			case <-stop:
				return
			default:
			}
			metrics.Read(s)
			if v := s[0].Value.Uint64(); v > peak.Load() {
				peak.Store(v)
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()
	fn()
	close(stop)
	<-done
	return peak.Load()
}

// Mirrors the heaviest store in production: 51k task records. Reports the peak
// live heap, which is the only thing that shows this change — B/op counts every
// byte allocated, and the line copies are allocated either way; what changes is
// whether they are alive at the same time.
//
// A peak assertion would be flaky across machines and GC timing, so this reports
// instead of failing. Measured against the real 31.7MB production store, five
// runs each: buffered 154 MiB median, streaming 101 MiB.
func BenchmarkLoadRecordsPeakHeap(b *testing.B) {
	const records = 51_000
	path := filepath.Join(b.TempDir(), "task-records.jsonl")
	f, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	for i := range records {
		fmt.Fprintf(f, `{"id":"task-%d","kind":"task","workspace_id":"ws","status":"new",`+
			`"summary":"%s","final_output":"%s","success":true}`+"\n", i,
			"a task summary with enough body for the file to reach a realistic size",
			"the final output of that task, also with some body behind it")
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}

	s := &Store{}
	var peak uint64
	b.ResetTimer()
	for range b.N {
		p := peakLiveHeap(func() {
			recs, e := s.loadRecordsFromPath(path)
			if e != nil {
				b.Fatal(e)
			}
			if len(recs) != records {
				b.Fatalf("loaded %d records, want %d", len(recs), records)
			}
		})
		if p > peak {
			peak = p
		}
	}
	b.ReportMetric(float64(peak)/(1<<20), "peak_MiB")
}
