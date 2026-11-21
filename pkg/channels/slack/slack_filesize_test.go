package slack

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	slacksdk "github.com/slack-go/slack"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/media"
)

func newMediaTestChannel(t *testing.T, body []byte) (*SlackChannel, string) {
	t.Helper()
	ch := &SlackChannel{BaseChannel: channels.NewBaseChannel("slack", nil, nil, nil)}
	ch.SetRunning(true)
	store := media.NewFileMediaStore()
	ch.SetMediaStore(store)
	localPath := filepath.Join(t.TempDir(), "report.txt")
	if err := os.WriteFile(localPath, body, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	ref, err := store.Store(localPath, media.MediaMeta{Filename: "report.txt", ContentType: "text/plain"}, "test-scope")
	if err != nil {
		t.Fatalf("Store() error = %v", err)
	}
	return ch, ref
}

// assertNotSent checks the error of a send that left the named files out: a
// permanent failure (channels.ErrSendFailed), which the channel manager does
// not retry, that names them. (seucaranguejo fork — MST-957)
func assertNotSent(t *testing.T, err error, names ...string) {
	t.Helper()
	if !errors.Is(err, channels.ErrSendFailed) {
		t.Fatalf("SendMedia() error = %v, want %v", err, channels.ErrSendFailed)
	}
	for _, name := range names {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("SendMedia() error = %q, want it to name %s", err, name)
		}
	}
}

func TestSendMedia_SetsFileSize(t *testing.T) {
	body := []byte("attachment body")
	ch, ref := newMediaTestChannel(t, body)
	var sizes []int
	ch.uploadFileFn = func(ctx context.Context, params slacksdk.UploadFileParameters) error {
		sizes = append(sizes, params.FileSize)
		return nil
	}
	_, err := ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
		ChatID: "C123456/1234567890.123456",
		Parts:  []bus.MediaPart{{Ref: ref, Type: "file", Filename: "report.txt", ContentType: "text/plain"}},
	})
	if err != nil {
		t.Fatalf("SendMedia() error = %v", err)
	}
	if len(sizes) != 1 || sizes[0] != len(body) {
		t.Fatalf("FileSize = %v, want [%d]", sizes, len(body))
	}
}

func TestSendMedia_SkipsEmptyFile(t *testing.T) {
	ch, ref := newMediaTestChannel(t, nil)
	uploads := 0
	ch.uploadFileFn = func(ctx context.Context, params slacksdk.UploadFileParameters) error {
		uploads++
		return nil
	}
	_, err := ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
		ChatID: "C123456",
		Parts:  []bus.MediaPart{{Ref: ref, Type: "file", Filename: "report.txt", ContentType: "text/plain"}},
	})
	if uploads != 0 {
		t.Fatalf("uploads = %d, want 0 for an empty file", uploads)
	}
	assertNotSent(t, err, "report.txt", "sent 0 of 1")
}

// goneMediaStore resolves every ref to a path that does not exist, so the
// os.Stat guard in SendMedia is the only thing that can stop the upload.
type goneMediaStore struct {
	media.MediaStore
	gone string
}

func (s goneMediaStore) Resolve(ref string) (string, error) {
	return s.gone, nil
}

func TestSendMedia_SkipsMissingFile(t *testing.T) {
	ch, ref := newMediaTestChannel(t, []byte("attachment body"))
	ch.SetMediaStore(goneMediaStore{
		MediaStore: ch.GetMediaStore(),
		gone:       filepath.Join(t.TempDir(), "gone.txt"),
	})
	uploads := 0
	ch.uploadFileFn = func(ctx context.Context, params slacksdk.UploadFileParameters) error {
		uploads++
		return nil
	}
	_, err := ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
		ChatID: "C123456",
		Parts:  []bus.MediaPart{{Ref: ref, Type: "file", Filename: "report.txt", ContentType: "text/plain"}},
	})
	if uploads != 0 {
		t.Fatalf("uploads = %d, want 0 for a missing file", uploads)
	}
	assertNotSent(t, err, "report.txt")
}

// A batch with an empty file sends the other files, and the caption, and
// reports only the one it left out. (seucaranguejo fork — MST-957)
func TestSendMedia_SendsTheRestAndReportsTheEmptyFile(t *testing.T) {
	ch, ref := newMediaTestChannel(t, []byte("attachment body"))
	emptyPath := filepath.Join(t.TempDir(), "empty.txt")
	if err := os.WriteFile(emptyPath, nil, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	emptyRef, err := ch.GetMediaStore().Store(emptyPath, media.MediaMeta{Filename: "empty.txt"}, "test-scope")
	if err != nil {
		t.Fatalf("Store() error = %v", err)
	}
	var uploaded, posted []string
	ch.uploadFileFn = func(ctx context.Context, params slacksdk.UploadFileParameters) error {
		uploaded = append(uploaded, params.Filename)
		return nil
	}
	ch.postTextFn = func(ctx context.Context, channelID, threadTS, text string) error {
		posted = append(posted, text)
		return nil
	}

	_, err = ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
		ChatID: "C123456",
		Parts: []bus.MediaPart{
			{Ref: emptyRef, Type: "file", Filename: "empty.txt"},
			{Ref: ref, Type: "file", Filename: "report.txt", Caption: "the report"},
		},
	})

	if !reflect.DeepEqual(uploaded, []string{"report.txt"}) || !reflect.DeepEqual(posted, []string{"the report"}) {
		t.Fatalf("uploaded %v and posted %v, want report.txt and its caption", uploaded, posted)
	}
	assertNotSent(t, err, "empty.txt", "sent 1 of 2")
	if strings.Contains(err.Error(), "report.txt") {
		t.Fatalf("SendMedia() error = %q, names the file it sent", err)
	}
}

// When no file goes up the caption does not either, and a file without a name
// is reported by its ref. (seucaranguejo fork — MST-957)
func TestSendMedia_PostsNoCaptionWhenNoFileWentUp(t *testing.T) {
	ch, ref := newMediaTestChannel(t, nil)
	unnamedPath := filepath.Join(t.TempDir(), "unnamed")
	if err := os.WriteFile(unnamedPath, nil, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	unnamedRef, err := ch.GetMediaStore().Store(unnamedPath, media.MediaMeta{}, "test-scope")
	if err != nil {
		t.Fatalf("Store() error = %v", err)
	}
	uploads, posts := 0, 0
	ch.uploadFileFn = func(ctx context.Context, params slacksdk.UploadFileParameters) error {
		uploads++
		return nil
	}
	ch.postTextFn = func(ctx context.Context, channelID, threadTS, text string) error {
		posts++
		return nil
	}

	_, err = ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
		ChatID: "C123456",
		Parts: []bus.MediaPart{
			{Ref: ref, Type: "file", Filename: "report.txt", Caption: "the report"},
			{Ref: unnamedRef, Type: "file"},
		},
	})

	if uploads != 0 || posts != 0 {
		t.Fatalf("uploads = %d, posts = %d, want none", uploads, posts)
	}
	assertNotSent(t, err, "sent 0 of 2", "report.txt, "+unnamedRef)
}
