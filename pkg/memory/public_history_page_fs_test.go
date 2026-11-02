package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestPublicHistoryPagesReadOnlyFSSeeksBeyondEightMiBAndPinsBothDirections(t *testing.T) {
	store, key := pagedStore(t, 0)
	messages := make([]providers.Message, 1752)
	for index := range messages {
		messages[index] = providers.Message{Role: "assistant", Content: fmt.Sprintf("row-%d %s", index, strings.Repeat("x", 5<<10))}
	}
	if err := store.SetHistory(t.Context(), key, messages); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(store.publicHistoryPath(key))
	if err != nil || info.Size() <= 8<<20 {
		t.Fatal("fixture must exceed the retired global byte limit", err)
	}
	request := PublicHistoryPageRequest{Key: key}
	traced := &publicPageTraceFS{FS: os.DirFS(store.dir)}
	first, err := ReadPublicHistoryPageFS(t.Context(), traced, request)
	if err != nil || first.Start != 1732 || first.End != 1752 || first.Stats.RowsRead != 20 ||
		traced.sourceBytes != first.Stats.DataBytesRead || traced.indexBytes != first.Stats.IndexBytesRead || traced.sourceBytes > 128<<10 {
		t.Fatalf("unbounded read-only recent page: %+v, %v", first, err)
	}
	t.Logf("read-only1752/over8MiB: source=%d index=%d metadata=%d rows=%d", traced.sourceBytes, traced.indexBytes, traced.otherBytes, first.Stats.RowsRead)
	if err := store.AddMessage(t.Context(), key, "assistant", "outside the frozen view"); err != nil {
		t.Fatal(err)
	}
	before := publicPageFiles(t, store.dir)
	page, seen := first, len(first.Entries)
	for page.OlderCursor != "" {
		previous := page.Start
		page, err = ReadPublicHistoryPageFS(t.Context(), os.DirFS(store.dir), PublicHistoryPageRequest{Key: key, Cursor: page.OlderCursor})
		if err != nil || page.Start >= previous || page.End != previous || page.Ceiling != 1752 {
			t.Fatalf("older read incorporated append or stopped advancing: %+v %v", page, err)
		}
		seen += len(page.Entries)
	}
	if seen != 1752 || page.Start != 0 {
		t.Fatal("read-only traversal lost messages", seen)
	}
	for page.NewerCursor != "" {
		previous := page.End
		page, err = ReadPublicHistoryPageFS(t.Context(), os.DirFS(store.dir), PublicHistoryPageRequest{Key: key, Cursor: page.NewerCursor})
		if err != nil || page.End <= previous || page.Start != previous || page.End > 1752 {
			t.Fatalf("newer read incorporated append or stopped advancing: %+v %v", page, err)
		}
	}
	if !reflect.DeepEqual(before, publicPageFiles(t, store.dir)) {
		t.Fatal("read-only pagination changed producer files")
	}
}

func TestPublicHistoryPagesReadOnlyFSMissingIndexAndRecoveryNeverWrite(t *testing.T) {
	for _, scenario := range []string{"index", "journal", "metadata"} {
		t.Run(scenario, func(t *testing.T) {
			store, key := pagedStore(t, 80)
			expected := ErrPublicHistoryCoverage
			switch scenario {
			case "index":
				if err := os.Remove(store.publicIndexPath(key)); err != nil {
					t.Fatal(err)
				}
				expected = ErrPublicHistoryIndexPreparing
			case "journal":
				if err := os.WriteFile(store.historyRecoveryPath(key), []byte("unsettled"), 0o600); err != nil {
					t.Fatal(err)
				}
				expected = ErrHistoryRecoveryRequired
			case "metadata":
				meta := mustPublicMeta(t, store, key)
				meta.PublicHistoryRevision = 0
				data, err := json.Marshal(meta)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(store.metaPath(key), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := publicPageFiles(t, store.dir)
			_, err := ReadPublicHistoryPageFS(t.Context(), os.DirFS(store.dir), PublicHistoryPageRequest{Key: key})
			if !errors.Is(err, expected) || !reflect.DeepEqual(before, publicPageFiles(t, store.dir)) {
				t.Fatal("read-only failure changed source or became success", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ReadPublicHistoryPageFS(ctx, os.DirFS(t.TempDir()), PublicHistoryPageRequest{Key: "missing"}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation attempted filesystem work", err)
	}
}

func publicPageFiles(t *testing.T, directory string) map[string]string {
	t.Helper()
	names, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	files := make(map[string]string, len(names))
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(directory, name.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[name.Name()] = string(data)
	}
	return files
}

type publicPageTraceFS struct {
	fs.FS
	sourceBytes, indexBytes, otherBytes int64
}

func (files *publicPageTraceFS) Open(name string) (fs.File, error) {
	file, err := files.FS.Open(name)
	if err != nil {
		return nil, err
	}
	return &publicPageTraceFile{File: file, name: name, trace: files}, nil
}

type publicPageTraceFile struct {
	fs.File
	name  string
	trace *publicPageTraceFS
}

func (file *publicPageTraceFile) Read(data []byte) (int, error) {
	if strings.HasSuffix(file.name, ".public") || strings.HasSuffix(file.name, ".public.index") {
		return 0, errors.New("public source must use indexed reads")
	}
	count, err := file.File.Read(data)
	file.trace.otherBytes += int64(count)
	return count, err
}

func (file *publicPageTraceFile) ReadAt(data []byte, offset int64) (int, error) {
	reader, ok := file.File.(io.ReaderAt)
	if !ok {
		return 0, ErrHistoryUnsupported
	}
	count, err := reader.ReadAt(data, offset)
	if strings.HasSuffix(file.name, ".public.index") {
		file.trace.indexBytes += int64(count)
	} else {
		file.trace.sourceBytes += int64(count)
	}
	return count, err
}
