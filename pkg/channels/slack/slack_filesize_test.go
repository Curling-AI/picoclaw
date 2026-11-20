package slack

import (
	"context"
	"os"
	"path/filepath"
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
	_, _ = ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
		ChatID: "C123456",
		Parts:  []bus.MediaPart{{Ref: ref, Type: "file", Filename: "report.txt", ContentType: "text/plain"}},
	})
	if uploads != 0 {
		t.Fatalf("uploads = %d, want 0 for an empty file", uploads)
	}
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
	_, _ = ch.SendMedia(context.Background(), bus.OutboundMediaMessage{
		ChatID: "C123456",
		Parts:  []bus.MediaPart{{Ref: ref, Type: "file", Filename: "report.txt", ContentType: "text/plain"}},
	})
	if uploads != 0 {
		t.Fatalf("uploads = %d, want 0 for a missing file", uploads)
	}
}
