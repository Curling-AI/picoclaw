package whatsapp

import (
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
)

func init() {
	channels.RegisterFactory(
		config.ChannelWhatsApp,
		func(channelName, channelType string, cfg *config.Config, b *bus.MessageBus) (channels.Channel, error) {
			bc := cfg.Channels[channelName]
			decoded, err := bc.GetDecoded()
			if err != nil {
				return nil, err
			}
			c, ok := decoded.(*config.WhatsAppSettings)
			if !ok {
				return nil, channels.ErrSendFailed
			}
			// Cloud API vence quando há credencial: é o caminho que não segura
			// conexão. A bridge fica para quem ainda roda uma.
			if c.CloudAPIReady() {
				return NewCloudChannel(bc, c, b)
			}
			return NewWhatsAppChannel(bc, c, b)
		},
	)
}
