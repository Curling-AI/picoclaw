package channels

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
)

// O portão exigia BridgeURL e descartava EM SILÊNCIO todo canal configurado com
// credencial da Cloud API — que é exatamente o que o formulário do Ethos
// coleta. Quatro assistentes em produção ficaram sem receber nada por causa
// disto, e o pod não registrava nem um aviso.
func TestWhatsAppProntoComCloudAPI(t *testing.T) {
	cloud := &config.WhatsAppSettings{PhoneNumberID: "123"}
	cloud.AccessToken = *config.NewSecureString("tok")

	semNumero := &config.WhatsAppSettings{}
	semNumero.AccessToken = *config.NewSecureString("tok")

	cases := []struct {
		nome   string
		cfg    *config.WhatsAppSettings
		pronto bool
	}{
		{"cloud api completa", cloud, true},
		{"bridge, como antes", &config.WhatsAppSettings{BridgeURL: "ws://bridge:8080"}, true},
		{"token sem número", semNumero, false},
		{"número sem token", &config.WhatsAppSettings{PhoneNumberID: "123"}, false},
		{"nada configurado", &config.WhatsAppSettings{}, false},
	}

	for _, c := range cases {
		t.Run(c.nome, func(t *testing.T) {
			pronto := c.cfg.CloudAPIReady() || c.cfg.BridgeURL != ""
			if pronto != c.pronto {
				t.Errorf("pronto = %v, queria %v", pronto, c.pronto)
			}
		})
	}
}
