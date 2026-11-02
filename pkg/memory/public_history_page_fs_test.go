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
		messages[index] = providers.Message{
			Role:    "assistant",
			Content: fmt.Sprintf("row-%d %s", index, strings.Repeat("x", 5<<10)),
		}
	}
	if operationErr0 := store.SetHistory(t.Context(), key, messages); operationErr0 != nil {
		t.Fatal(operationErr0)
	}
	info, operationErr1 := os.Stat(store.publicHistoryPath(key))
	if operationErr1 != nil || info.Size() <= 8<<20 {
		t.Fatal("fixture must exceed the retired global byte limit", operationErr1)
	}
	request := PublicHistoryPageRequest{Key: key}
	traced := &publicPageTraceFS{FS: os.DirFS(store.dir)}
	first, operationErr1 := ReadPublicHistoryPageFS(t.Context(), traced, request)
	if operationErr1 != nil || first.Start != 1732 || first.End != 1752 || first.Stats.RowsRead != 20 ||
		traced.sourceBytes != first.Stats.DataBytesRead || traced.indexBytes != first.Stats.IndexBytesRead || traced.sourceBytes > 128<<10 {
		t.Fatalf("unbounded read-only recent page: %+v, %v", first, operationErr1)
	}
	t.Logf(
		"read-only1752/over8MiB: source=%d index=%d metadata=%d rows=%d",
		traced.sourceBytes,
		traced.indexBytes,
		traced.otherBytes,
		first.Stats.RowsRead,
	)
	if operationErr2 := store.AddMessage(
		t.Context(),
		key,
		"assistant",
		"outside the frozen view",
	); operationErr2 != nil {
		t.Fatal(operationErr2)
	}
	before := publicPageFiles(t, store.dir)
	page, seen := first, len(first.Entries)
	for page.OlderCursor != "" {
		previous := page.Start
		page, operationErr1 = ReadPublicHistoryPageFS(
			t.Context(),
			os.DirFS(store.dir),
			PublicHistoryPageRequest{Key: key, Cursor: page.OlderCursor},
		)
		if operationErr1 != nil || page.Start >= previous || page.End != previous || page.Ceiling != 1752 {
			t.Fatalf("older read incorporated append or stopped advancing: %+v %v", page, operationErr1)
		}
		seen += len(page.Entries)
	}
	if seen != 1752 || page.Start != 0 {
		t.Fatal("read-only traversal lost messages", seen)
	}
	for page.NewerCursor != "" {
		previous := page.End
		page, operationErr1 = ReadPublicHistoryPageFS(
			t.Context(),
			os.DirFS(store.dir),
			PublicHistoryPageRequest{Key: key, Cursor: page.NewerCursor},
		)
		if operationErr1 != nil || page.End <= previous || page.Start != previous || page.End > 1752 {
			t.Fatalf("newer read incorporated append or stopped advancing: %+v %v", page, operationErr1)
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
				if operationErr3 := os.Remove(store.publicIndexPath(key)); operationErr3 != nil {
					t.Fatal(operationErr3)
				}
				expected = ErrPublicHistoryIndexPreparing
			case "journal":
				if operationErr4 := os.WriteFile(
					store.historyRecoveryPath(key),
					[]byte("unsettled"),
					0o600,
				); operationErr4 != nil {
					t.Fatal(operationErr4)
				}
				expected = ErrHistoryRecoveryRequired
			case "metadata":
				meta := mustPublicMeta(t, store, key)
				meta.PublicHistoryRevision = 0
				data, operationErr5 := json.Marshal(meta)
				if operationErr5 != nil {
					t.Fatal(operationErr5)
				}
				if operationErr6 := os.WriteFile(store.metaPath(key), data, 0o600); operationErr6 != nil {
					t.Fatal(operationErr6)
				}
			}
			before := publicPageFiles(t, store.dir)
			_, operationErr7 := ReadPublicHistoryPageFS(
				t.Context(),
				os.DirFS(store.dir),
				PublicHistoryPageRequest{Key: key},
			)
			if !errors.Is(operationErr7, expected) || !reflect.DeepEqual(before, publicPageFiles(t, store.dir)) {
				t.Fatal("read-only failure changed source or became success", operationErr7)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, operationErr8 := ReadPublicHistoryPageFS(
		ctx,
		os.DirFS(t.TempDir()),
		PublicHistoryPageRequest{Key: "missing"},
	); !errors.Is(
		operationErr8,
		context.Canceled,
	) {
		t.Fatal("cancellation attempted filesystem work", operationErr8)
	}
}

func TestPublicHistoryIndexPreparationBoundsSourceIOSeparatelyFromCommittedRows(t *testing.T) {
	for _, size := range []int{5 << 10, 393 << 10} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			store, key := pagedStore(t, 0)
			messages := make([]providers.Message, 80)
			for index := range messages {
				messages[index] = providers.Message{Role: "assistant", Content: strings.Repeat("x", size)}
			}
			if operationErr9 := store.SetHistory(t.Context(), key, messages); operationErr9 != nil {
				t.Fatal(operationErr9)
			}
			meta := mustPublicMeta(t, store, key)
			state, operationErr10 := store.readPublicIndexPreparation(key, meta)
			if operationErr10 != nil {
				t.Fatal(operationErr10)
			}
			trace := &publicPageTraceFS{FS: os.DirFS(store.dir)}
			source, operationErr10 := openPublicHistoryReadFile(trace, filepath.Base(store.publicHistoryPath(key)))
			if operationErr10 != nil {
				t.Fatal(operationErr10)
			}
			next, records, operationErr10 := readPublicIndexPreparationBatch(t.Context(), source, state, meta)
			if closeErr := source.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if operationErr10 != nil || next.Count == 0 || next.Count > 50 || next.Count >= meta.PublicHistoryCount ||
				trace.sourceBytes > PublicHistoryOperationBytes || next.Byte > trace.sourceBytes ||
				len(records) != int(next.Count)*publicIndexRecordBytes {
				t.Fatalf(
					"unbounded preparation: state=%+v io=%d records=%d err=%v",
					next,
					trace.sourceBytes,
					len(records),
					operationErr10,
				)
			}
			t.Logf(
				"preparation: source=%d committedBytes=%d committedRows=%d indexBytes=%d",
				trace.sourceBytes,
				next.Byte,
				next.Count,
				len(records),
			)
		})
	}
}

func publicPageFiles(t *testing.T, directory string) map[string]string {
	t.Helper()
	names, operationErr11 := os.ReadDir(directory)
	if operationErr11 != nil {
		t.Fatal(operationErr11)
	}
	files := make(map[string]string, len(names))
	for _, name := range names {
		data, operationErr12 := os.ReadFile(filepath.Join(directory, name.Name()))
		if operationErr12 != nil {
			t.Fatal(operationErr12)
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
	file, operationErr13 := files.FS.Open(name)
	if operationErr13 != nil {
		return nil, operationErr13
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
	count, operationErr14 := file.File.Read(data)
	file.trace.otherBytes += int64(count)
	return count, operationErr14
}

func (file *publicPageTraceFile) ReadAt(data []byte, offset int64) (int, error) {
	reader, ok := file.File.(io.ReaderAt)
	if !ok {
		return 0, ErrHistoryUnsupported
	}
	count, operationErr15 := reader.ReadAt(data, offset)
	if strings.HasSuffix(file.name, ".public.index") {
		file.trace.indexBytes += int64(count)
	} else {
		file.trace.sourceBytes += int64(count)
	}
	return count, operationErr15
}
