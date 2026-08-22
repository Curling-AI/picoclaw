package whatsapp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
)

func cloudFor(t *testing.T, base string) *CloudChannel {
	t.Helper()
	cfg := &config.WhatsAppSettings{PhoneNumberID: "1234567890", GraphBaseURL: base}
	cfg.AccessToken = *config.NewSecureString("tok")
	c, err := NewCloudChannel(&config.Channel{}, cfg, bus.NewMessageBus())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	return c
}

// The reply has to reach the Cloud API in the shape Meta expects, addressed to
// the number the inbound webhook carried.
func TestCloudSend(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"messages":[{"id":"wamid.x"}]}`))
	}))
	defer srv.Close()

	c := cloudFor(t, srv.URL)
	if _, err := c.Send(context.Background(), bus.OutboundMessage{ChatID: "5511999999999", Content: "oi"}); err != nil {
		t.Fatalf("send: %v", err)
	}

	if gotPath != "/1234567890/messages" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("authorization = %q", gotAuth)
	}
	if gotBody["to"] != "5511999999999" || gotBody["messaging_product"] != "whatsapp" {
		t.Errorf("corpo = %+v", gotBody)
	}
}

// 5xx e 429 são temporários: o manager tem retry com backoff e reenviar resolve.
// 4xx é nosso erro e reenviar só repete a recusa.
func TestCloudSendClassificaFalhas(t *testing.T) {
	cases := []struct {
		status    int
		temporary bool
	}{
		{http.StatusInternalServerError, true},
		{http.StatusTooManyRequests, true},
		{http.StatusUnauthorized, false},
		{http.StatusBadRequest, false},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(`{"error":{"message":"segredo no corpo"}}`))
		}))
		ch := cloudFor(t, srv.URL)
		_, err := ch.Send(context.Background(), bus.OutboundMessage{ChatID: "55", Content: "oi"})
		srv.Close()

		if err == nil {
			t.Errorf("status %d devia falhar", c.status)
			continue
		}
		if got := strings.Contains(err.Error(), channels.ErrTemporary.Error()); got != c.temporary {
			t.Errorf("status %d: temporário=%v, queria %v (%v)", c.status, got, c.temporary, err)
		}
		if strings.Contains(err.Error(), "segredo no corpo") {
			t.Errorf("status %d vazou o corpo da resposta: %v", c.status, err)
		}
	}
}

// Sem destinatário não há o que fazer, e mandar assim vira 400 na Meta.
func TestCloudSendSemDestinatario(t *testing.T) {
	c := cloudFor(t, "http://nao-usado")
	if _, err := c.Send(context.Background(), bus.OutboundMessage{Content: "oi"}); err == nil {
		t.Error("devia recusar sem ChatID")
	}
}
