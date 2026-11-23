package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	slacksdk "github.com/slack-go/slack"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/media"
)

// Tests of the retry in place of SendMedia: once a file is up, a step that
// fails (an upload, the caption) is retried alone, and a send that cannot go
// on is a permanent failure, so the channel manager does not send the batch
// again. (seucaranguejo fork — MST-957)

// errSlackServer is what slack-go returns for a 500 on a step of the upload.
var errSlackServer = fmt.Errorf("CompleteUploadExternal: %w",
	slacksdk.StatusCodeError{Code: http.StatusInternalServerError, Status: "500 Internal Server Error"})

func slackRateLimited(retryAfter time.Duration) error {
	return fmt.Errorf("CompleteUploadExternal: %w", &slacksdk.RateLimitedError{RetryAfter: retryAfter})
}

// storeMediaFile stores a file with body under name and returns its part.
func storeMediaFile(t *testing.T, store media.MediaStore, name, caption string) bus.MediaPart {
	t.Helper()
	localPath := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(localPath, []byte(name+" body"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	ref, err := store.Store(localPath, media.MediaMeta{Filename: name}, "test-scope")
	if err != nil {
		t.Fatalf("Store() error = %v", err)
	}
	return bus.MediaPart{Ref: ref, Type: "file", Filename: name, Caption: caption}
}

// newRetryTestChannel is a running channel with a real media store that
// records the waits of its retries in place and skips them.
func newRetryTestChannel(t *testing.T) (*SlackChannel, *[]time.Duration) {
	t.Helper()
	ch := &SlackChannel{BaseChannel: channels.NewBaseChannel("slack", nil, nil, nil)}
	ch.SetRunning(true)
	ch.SetMediaStore(media.NewFileMediaStore())
	return ch, recordRetryWaits(ch)
}

func recordRetryWaits(ch *SlackChannel) *[]time.Duration {
	waits := []time.Duration{}
	ch.mediaRetryWait = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return ctx.Err()
	}
	return &waits
}

// failingUploads counts the uploads per filename and fails them with
// fail(filename, try), try counting from 1; a nil error lets it go up.
func failingUploads(ch *SlackChannel, fail func(name string, try int) error) map[string]int {
	tries := map[string]int{}
	ch.uploadFileFn = func(ctx context.Context, params slacksdk.UploadFileParameters) error {
		tries[params.Filename]++
		return fail(params.Filename, tries[params.Filename])
	}
	return tries
}

func sendParts(ch *SlackChannel, ctx context.Context, parts ...bus.MediaPart) error {
	_, err := ch.SendMedia(ctx, bus.OutboundMediaMessage{ChatID: "C123456/1234567890.123456", Parts: parts})
	return err
}

// assertErrorText checks the whole text the agent reads for a send that left
// something out.
func assertErrorText(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || err.Error() != want {
		t.Fatalf("SendMedia() error = %v, want %q", err, want)
	}
}

func assertWaits(t *testing.T, waits *[]time.Duration, want ...time.Duration) {
	t.Helper()
	if want == nil {
		want = []time.Duration{}
	}
	if !reflect.DeepEqual(*waits, want) {
		t.Fatalf("retry waits = %v, want %v", *waits, want)
	}
}

func TestSendMedia_RetriesOnlyTheUploadThatFailed(t *testing.T) {
	ch, waits := newRetryTestChannel(t)
	a := storeMediaFile(t, ch.GetMediaStore(), "a.txt", "")
	b := storeMediaFile(t, ch.GetMediaStore(), "b.txt", "")
	tries := failingUploads(ch, func(name string, try int) error {
		if name == "b.txt" && try <= 2 {
			return errSlackServer
		}
		return nil
	})

	if err := sendParts(ch, context.Background(), a, b); err != nil {
		t.Fatalf("SendMedia() error = %v", err)
	}
	if want := map[string]int{"a.txt": 1, "b.txt": 3}; !reflect.DeepEqual(tries, want) {
		t.Fatalf("upload tries = %v, want %v", tries, want)
	}
	assertWaits(t, waits, 500*time.Millisecond, time.Second)
}

func TestSendMedia_ReportsWhatWentUpWhenAnUploadKeepsFailing(t *testing.T) {
	ch, waits := newRetryTestChannel(t)
	gone := storeMediaFile(t, ch.GetMediaStore(), "gone.txt", "")
	gonePath, err := ch.GetMediaStore().Resolve(gone.Ref)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if err = os.Remove(gonePath); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	a := storeMediaFile(t, ch.GetMediaStore(), "a.txt", "the files")
	b := storeMediaFile(t, ch.GetMediaStore(), "b.txt", "")
	c := storeMediaFile(t, ch.GetMediaStore(), "c.txt", "")
	tries := failingUploads(ch, func(name string, try int) error {
		if name == "b.txt" {
			return errSlackServer
		}
		return nil
	})
	posts := 0
	ch.postTextFn = func(ctx context.Context, channelID, threadTS, text string) error {
		posts++
		return nil
	}

	err = sendParts(ch, context.Background(), gone, a, b, c)

	if want := map[string]int{"a.txt": 1, "b.txt": 4}; !reflect.DeepEqual(tries, want) {
		t.Fatalf("upload tries = %v, want %v", tries, want)
	}
	if posts != 0 {
		t.Fatalf("caption posts = %d, want none once the send stopped", posts)
	}
	assertWaits(t, waits, 500*time.Millisecond, time.Second, 2*time.Second)
	assertNotSent(t, err,
		"sent 1 of 4 files",
		"upload of b.txt failed, not sent: b.txt, c.txt, the caption",
		"missing, empty or unreadable, not sent: gone.txt",
		"the files sent are in the conversation, do not send them again")
	if strings.Contains(err.Error(), "a.txt") {
		t.Fatalf("SendMedia() error = %q, names the file it sent", err)
	}
	assertErrorText(t, err, "slack send media: sent 1 of 4 files; "+
		"upload of b.txt failed, not sent: b.txt, c.txt, the caption; "+
		"missing, empty or unreadable, not sent: gone.txt; "+
		"the files sent are in the conversation, do not send them again: send failed")
}

func TestSendMedia_RetriesOnlyTheCaption(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failures  int
		wantPosts int
		wantErr   bool
		wantWaits []time.Duration
	}{
		{name: "fails twice", failures: 2, wantPosts: 3, wantWaits: []time.Duration{500 * time.Millisecond, time.Second}},
		{name: "keeps failing", failures: 10, wantPosts: 4, wantErr: true, wantWaits: []time.Duration{
			500 * time.Millisecond, time.Second, 2 * time.Second,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ch, waits := newRetryTestChannel(t)
			a := storeMediaFile(t, ch.GetMediaStore(), "a.txt", "the files")
			b := storeMediaFile(t, ch.GetMediaStore(), "b.txt", "")
			tries := failingUploads(ch, func(string, int) error { return nil })
			posts := 0
			ch.postTextFn = func(ctx context.Context, channelID, threadTS, text string) error {
				posts++
				if posts <= tc.failures {
					return errSlackServer
				}
				return nil
			}

			err := sendParts(ch, context.Background(), a, b)

			if want := map[string]int{"a.txt": 1, "b.txt": 1}; !reflect.DeepEqual(tries, want) {
				t.Fatalf("upload tries = %v, want %v", tries, want)
			}
			if posts != tc.wantPosts {
				t.Fatalf("caption posts = %d, want %d", posts, tc.wantPosts)
			}
			assertWaits(t, waits, tc.wantWaits...)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("SendMedia() error = %v", err)
				}
				return
			}
			assertNotSent(t, err, "sent 2 of 2 files", "posting the caption failed, not sent: the caption",
				"do not send them again")
			assertErrorText(t, err, "slack send media: sent 2 of 2 files; "+
				"posting the caption failed, not sent: the caption; "+
				"the files sent are in the conversation, do not send them again: send failed")
		})
	}
}

// Before any file is up, the manager's retry of the batch sends nothing twice,
// so the channel leaves it the retry, as before. (seucaranguejo fork — MST-957)
func TestSendMedia_LeavesTheFirstUploadToTheManager(t *testing.T) {
	ch, waits := newRetryTestChannel(t)
	a := storeMediaFile(t, ch.GetMediaStore(), "a.txt", "")
	b := storeMediaFile(t, ch.GetMediaStore(), "b.txt", "")
	tries := failingUploads(ch, func(name string, try int) error {
		if name == "a.txt" {
			return errSlackServer
		}
		return nil
	})

	err := sendParts(ch, context.Background(), a, b)

	if !errors.Is(err, channels.ErrTemporary) || errors.Is(err, channels.ErrSendFailed) {
		t.Fatalf("SendMedia() error = %v, want %v for the manager to retry", err, channels.ErrTemporary)
	}
	if want := map[string]int{"a.txt": 1}; !reflect.DeepEqual(tries, want) {
		t.Fatalf("upload tries = %v, want %v", tries, want)
	}
	assertWaits(t, waits)
}

func TestSendMedia_WaitsTheRetryAfterOfARateLimit(t *testing.T) {
	ch, waits := newRetryTestChannel(t)
	a := storeMediaFile(t, ch.GetMediaStore(), "a.txt", "")
	b := storeMediaFile(t, ch.GetMediaStore(), "b.txt", "")
	tries := failingUploads(ch, func(name string, try int) error {
		switch {
		case name != "b.txt":
			return nil
		case try == 1:
			return slackRateLimited(3 * time.Second)
		case try == 2:
			// A Retry-After shorter than the backoff waits the backoff.
			return slackRateLimited(0)
		case try == 3:
			return slackRateLimited(slackMediaMaxWait)
		}
		return nil
	})

	if err := sendParts(ch, context.Background(), a, b); err != nil {
		t.Fatalf("SendMedia() error = %v", err)
	}
	if want := map[string]int{"a.txt": 1, "b.txt": 4}; !reflect.DeepEqual(tries, want) {
		t.Fatalf("upload tries = %v, want %v", tries, want)
	}
	assertWaits(t, waits, 3*time.Second, time.Second, slackMediaMaxWait)
}

func TestSendMedia_GivesUpOnARetryAfterLongerThanTheCap(t *testing.T) {
	ch, waits := newRetryTestChannel(t)
	a := storeMediaFile(t, ch.GetMediaStore(), "a.txt", "")
	b := storeMediaFile(t, ch.GetMediaStore(), "b.txt", "")
	tries := failingUploads(ch, func(name string, try int) error {
		if name == "b.txt" {
			return slackRateLimited(slackMediaMaxWait + time.Second)
		}
		return nil
	})

	err := sendParts(ch, context.Background(), a, b)

	if want := map[string]int{"a.txt": 1, "b.txt": 1}; !reflect.DeepEqual(tries, want) {
		t.Fatalf("upload tries = %v, want %v", tries, want)
	}
	assertWaits(t, waits)
	assertNotSent(t, err, "sent 1 of 2 files", "upload of b.txt failed")
	// No caption and no file left out: the text names only b.txt.
	assertErrorText(t, err, "slack send media: sent 1 of 2 files; upload of b.txt failed, not sent: b.txt; "+
		"the files sent are in the conversation, do not send them again: send failed")
}

// The wait of a retry in place ends with the send's context.
func TestSendMedia_StopsRetryingWhenTheContextEnds(t *testing.T) {
	ch := &SlackChannel{BaseChannel: channels.NewBaseChannel("slack", nil, nil, nil)}
	ch.SetRunning(true)
	ch.SetMediaStore(media.NewFileMediaStore())
	a := storeMediaFile(t, ch.GetMediaStore(), "a.txt", "")
	b := storeMediaFile(t, ch.GetMediaStore(), "b.txt", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tries := failingUploads(ch, func(name string, try int) error {
		if name == "a.txt" {
			cancel()
			return nil
		}
		return errSlackServer
	})

	start := time.Now()
	err := sendParts(ch, ctx, a, b)

	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("SendMedia() took %v after the context ended, want no wait", elapsed)
	}
	if want := map[string]int{"a.txt": 1, "b.txt": 1}; !reflect.DeepEqual(tries, want) {
		t.Fatalf("upload tries = %v, want %v", tries, want)
	}
	assertNotSent(t, err, "sent 1 of 2 files", "upload of b.txt failed")
}

func TestWaitMediaRetry_WaitsOnATimer(t *testing.T) {
	ch := &SlackChannel{}
	if err := ch.waitMediaRetry(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("waitMediaRetry() error = %v, want nil", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ch.waitMediaRetry(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitMediaRetry() error = %v, want %v", err, context.Canceled)
	}
}

// fakeSlackFiles is the slice of the Slack Web API that SendMedia uses, for
// the real slack-go client: auth.test, files.getUploadURLExternal, the upload
// URL, files.completeUploadExternal (it shares the file in the conversation)
// and chat.postMessage. fail answers a share of the file name, or the post of
// "caption", with an HTTP status (and Retry-After, when not empty) on its try
// (from 1); status 0 lets it through.
type fakeSlackFiles struct {
	t      *testing.T
	srv    *httptest.Server
	fail   func(name string, try int) (status int, retryAfter string)
	mu     sync.Mutex
	names  map[string]string
	tries  map[string]int
	shared map[string]int
}

func newFakeSlackFiles(t *testing.T, fail func(name string, try int) (int, string)) *fakeSlackFiles {
	f := &fakeSlackFiles{
		t:      t,
		fail:   fail,
		names:  map[string]string{},
		tries:  map[string]int{},
		shared: map[string]int{},
	}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSlackFiles) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.HasPrefix(r.URL.Path, "/upload/") {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
		return
	}
	_ = r.ParseForm()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/auth.test":
		_, _ = w.Write([]byte(`{"ok":true,"user_id":"UBOT","team_id":"T1"}`))
	case "/api/files.getUploadURLExternal":
		id := fmt.Sprintf("F%d", len(f.names)+1)
		f.names[id] = r.PostForm.Get("filename")
		_, _ = fmt.Fprintf(w, `{"ok":true,"upload_url":%q,"file_id":%q}`, f.srv.URL+"/upload/"+id, id)
	case "/api/files.completeUploadExternal":
		var files []slacksdk.FileSummary
		if err := json.Unmarshal([]byte(r.PostForm.Get("files")), &files); err != nil || len(files) != 1 {
			f.t.Errorf("files.completeUploadExternal files = %q", r.PostForm.Get("files"))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		name := f.names[files[0].ID]
		if f.refuse(w, name) {
			return
		}
		f.shared[name]++
		_, _ = fmt.Fprintf(w, `{"ok":true,"files":[{"id":%q,"title":%q}]}`, files[0].ID, name)
	case "/api/chat.postMessage":
		if f.refuse(w, "caption") {
			return
		}
		f.shared["caption"]++
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C123456","ts":"1234567890.654321"}`))
	default:
		f.t.Errorf("unexpected Slack call %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeSlackFiles) refuse(w http.ResponseWriter, name string) bool {
	f.tries[name]++
	status, retryAfter := f.fail(name, f.tries[name])
	if status == 0 {
		return false
	}
	if retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	w.WriteHeader(status)
	return true
}

func (f *fakeSlackFiles) counts() (tries, shared map[string]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return copyCounts(f.tries), copyCounts(f.shared)
}

func copyCounts(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// The batch goes through the channel manager's sendMediaWithRetry, and the
// uploads through the real slack-go client, against fakeSlackFiles: no file
// and no caption reaches the conversation twice. (seucaranguejo fork — MST-957)
func TestManagerSendMedia_SendsNoFileTwice(t *testing.T) {
	for _, tc := range []struct {
		name       string
		fail       func(name string, try int) (int, string)
		wantErr    error
		wantShared map[string]int
		wantTries  map[string]int
		wantWaits  []time.Duration
	}{
		{
			name: "an upload keeps failing",
			fail: func(name string, try int) (int, string) {
				if name == "b.txt" {
					return http.StatusInternalServerError, ""
				}
				return 0, ""
			},
			wantErr:    channels.ErrSendFailed,
			wantShared: map[string]int{"a.txt": 1},
			wantTries:  map[string]int{"a.txt": 1, "b.txt": 4},
			wantWaits:  []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second},
		},
		{
			name: "a rate limit, then the upload goes",
			fail: func(name string, try int) (int, string) {
				if name == "b.txt" && try == 1 {
					return http.StatusTooManyRequests, "2"
				}
				return 0, ""
			},
			wantShared: map[string]int{"a.txt": 1, "b.txt": 1, "caption": 1},
			wantTries:  map[string]int{"a.txt": 1, "b.txt": 2, "caption": 1},
			wantWaits:  []time.Duration{2 * time.Second},
		},
		{
			name: "the caption keeps failing",
			fail: func(name string, try int) (int, string) {
				if name == "caption" {
					return http.StatusInternalServerError, ""
				}
				return 0, ""
			},
			wantErr:    channels.ErrSendFailed,
			wantShared: map[string]int{"a.txt": 1, "b.txt": 1},
			wantTries:  map[string]int{"a.txt": 1, "b.txt": 1, "caption": 4},
			wantWaits:  []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second},
		},
		{
			name: "nothing up yet, the manager retries",
			fail: func(name string, try int) (int, string) {
				if name == "a.txt" && try == 1 {
					return http.StatusInternalServerError, ""
				}
				return 0, ""
			},
			wantShared: map[string]int{"a.txt": 1, "b.txt": 1, "caption": 1},
			wantTries:  map[string]int{"a.txt": 2, "b.txt": 1, "caption": 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeSlackFiles(t, tc.fail)
			cfg := &config.SlackSettings{APIURL: fake.srv.URL + "/api"}
			cfg.BotToken = *config.NewSecureString("xoxb-test")
			ch, err := NewSlackChannel(&config.Channel{Type: "slack", Enabled: true}, cfg, bus.NewMessageBus())
			if err != nil {
				t.Fatalf("NewSlackChannel() error = %v", err)
			}
			waits := recordRetryWaits(ch)
			store := media.NewFileMediaStore()
			m, err := channels.NewManager(&config.Config{}, bus.NewMessageBus(), store)
			if err != nil {
				t.Fatalf("NewManager() error = %v", err)
			}
			m.RegisterChannel("slack", ch)
			ch.SetMediaStore(store)
			if err = m.StartAll(context.Background()); err != nil {
				t.Fatalf("StartAll() error = %v", err)
			}
			t.Cleanup(func() { _ = m.StopAll(context.Background()) })
			a := storeMediaFile(t, store, "a.txt", "the files")
			b := storeMediaFile(t, store, "b.txt", "")

			err = m.SendMedia(context.Background(), bus.OutboundMediaMessage{
				Channel: "slack",
				ChatID:  "C123456/1234567890.123456",
				Parts:   []bus.MediaPart{a, b},
			})

			tries, shared := fake.counts()
			if !reflect.DeepEqual(shared, tc.wantShared) {
				t.Fatalf("shared in the conversation = %v, want %v", shared, tc.wantShared)
			}
			if !reflect.DeepEqual(tries, tc.wantTries) {
				t.Fatalf("tries = %v, want %v", tries, tc.wantTries)
			}
			if tc.wantErr == nil && err != nil {
				t.Fatalf("SendMedia() error = %v", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("SendMedia() error = %v, want %v", err, tc.wantErr)
			}
			assertWaits(t, waits, tc.wantWaits...)
		})
	}
}
