package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
)

const (
	defaultGraphBase = "https://graph.facebook.com/v21.0"
	cloudSendTimeout = 20 * time.Second
)

// CloudChannel replies over Meta's Cloud API and receives nothing: inbound
// arrives at a webhook outside the pod.
//
// It is the WhatsApp twin of the Telegram send-only mode, and exists for the
// same reason — a channel holding a connection keeps the pod awake forever,
// which is the one thing the scale plan cannot afford. There is no socket here
// and nothing to reconnect: Start only checks the credentials are present.
type CloudChannel struct {
	*channels.BaseChannel

	token       string
	phoneNumber string
	base        string
	http        *http.Client
}

func NewCloudChannel(bc *config.Channel, cfg *config.WhatsAppSettings, b *bus.MessageBus) (*CloudChannel, error) {
	base := channels.NewBaseChannel(
		"whatsapp",
		cfg,
		b,
		bc.AllowFrom,
		channels.WithMaxMessageLength(4096),
		channels.WithReasoningChannelID(bc.ReasoningChannelID),
	)

	graph := strings.TrimSuffix(strings.TrimSpace(cfg.GraphBaseURL), "/")
	if graph == "" {
		graph = defaultGraphBase
	}

	return &CloudChannel{
		BaseChannel: base,
		token:       cfg.AccessToken.String(),
		phoneNumber: cfg.PhoneNumberID,
		base:        graph,
		http:        &http.Client{Timeout: cloudSendTimeout},
	}, nil
}

func (c *CloudChannel) Name() string { return "whatsapp" }

func (c *CloudChannel) Start(ctx context.Context) error {
	if c.token == "" || c.phoneNumber == "" {
		return fmt.Errorf("whatsapp cloud: access token and phone number id are required")
	}
	c.SetRunning(true)
	logger.InfoCF("whatsapp", "WhatsApp connected (send-only, Cloud API)", map[string]any{
		"phone_number_id": c.phoneNumber,
	})
	return nil
}

func (c *CloudChannel) Stop(context.Context) error {
	c.SetRunning(false)
	return nil
}

// Send posts the reply to the Cloud API. ChatID is the recipient's phone number
// in international format, which is what the inbound webhook carries.
func (c *CloudChannel) Send(ctx context.Context, msg bus.OutboundMessage) ([]string, error) {
	if !c.IsRunning() {
		return nil, channels.ErrNotRunning
	}
	if strings.TrimSpace(msg.ChatID) == "" {
		return nil, fmt.Errorf("whatsapp cloud: outbound without a recipient")
	}

	payload := map[string]any{
		"messaging_product": "whatsapp",
		"recipient_type":    "individual",
		"to":                msg.ChatID,
		"type":              "text",
		"text":              map[string]any{"body": msg.Content},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("whatsapp cloud: encoding the message: %w", err)
	}

	endpoint := fmt.Sprintf("%s/%s/messages", c.base, url.PathEscape(c.phoneNumber))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("whatsapp cloud: building the request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// Temporary: the manager retries with backoff.
		return nil, fmt.Errorf("whatsapp cloud: %w", channels.ErrTemporary)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("whatsapp cloud returned %d: %w", resp.StatusCode, channels.ErrTemporary)
	}
	if resp.StatusCode >= 400 {
		// 4xx is our fault (bad token, wrong number): retrying cannot fix it.
		// The body may carry the token, so it never reaches the error.
		return nil, fmt.Errorf("whatsapp cloud refused with %d", resp.StatusCode)
	}
	return nil, nil
}
