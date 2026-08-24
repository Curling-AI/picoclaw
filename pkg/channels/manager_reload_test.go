package channels

import (
	"context"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
)

// Reload precisa PARAR o canal que saiu do config, não só esquecê-lo.
//
// O caso que motivou o teste é a migração do Slack de Socket Mode para webhook:
// o canal muda de forma, o hash muda, e o antigo tem de largar o websocket. Se
// ele continuar de pé, o pod segue segurando a conexão que a migração existe
// para eliminar — e sem barulho nenhum, porque o canal novo também funciona.
//
// Reload tinha os helpers cobertos (toChannelHashes, compareChannels) mas o
// caminho added/removed não, e o control plane passou a depender dele.
func TestReloadParaOCanalQueSaiuDoConfig(t *testing.T) {
	antigo := config.DefaultConfig()
	antigo.Channels["slack"] = &config.Channel{
		Enabled:  true,
		Settings: config.RawNode(`{"app_token":"xapp-socket"}`),
	}

	parado := false
	ch := &mockChannel{stopFn: func(context.Context) error {
		parado = true
		return nil
	}}

	m := &Manager{
		channels:      map[string]Channel{"slack": ch},
		workers:       make(map[string]*channelWorker),
		bus:           bus.NewMessageBus(),
		config:        antigo,
		channelHashes: toChannelHashes(antigo),
	}

	// Config novo sem o canal: é o que o removed deve pegar.
	novo := config.DefaultConfig()
	if err := m.Reload(context.Background(), novo); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	if !parado {
		t.Error("o canal removido não recebeu Stop — o websocket antigo continua de pé")
	}
	if _, ainda := m.channels["slack"]; ainda {
		t.Error("o canal removido continua registrado no manager")
	}
}

// Canal que NÃO mudou não pode ser reiniciado.
//
// É o que separa "reconciliar" de "reiniciar tudo": derrubar um canal intacto
// numa mudança que não é dele custa uma janela de indisponibilidade sem motivo.
func TestReloadNaoTocaCanalIntacto(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Channels["slack"] = &config.Channel{
		Enabled:  true,
		Settings: config.RawNode(`{"bot_token":"xoxb-igual"}`),
	}

	parado := false
	ch := &mockChannel{stopFn: func(context.Context) error {
		parado = true
		return nil
	}}

	m := &Manager{
		channels:      map[string]Channel{"slack": ch},
		workers:       make(map[string]*channelWorker),
		bus:           bus.NewMessageBus(),
		config:        cfg,
		channelHashes: toChannelHashes(cfg),
	}

	// Mesmo canal, config recarregado: o hash não muda.
	igual := config.DefaultConfig()
	igual.Channels["slack"] = &config.Channel{
		Enabled:  true,
		Settings: config.RawNode(`{"bot_token":"xoxb-igual"}`),
	}
	if err := m.Reload(context.Background(), igual); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	if parado {
		t.Error("canal intacto foi parado — Reload está derrubando o que não mudou")
	}
	if _, ok := m.channels["slack"]; !ok {
		t.Error("canal intacto sumiu do manager")
	}
}

// O canal `grpc` é registrado pelo main, fora do config, e não pode sumir num
// reload: é por ele que a conversa da web recebe resposta.
func TestReloadPreservaCanalForaDoConfig(t *testing.T) {
	cfg := config.DefaultConfig()
	m := &Manager{
		channels:      map[string]Channel{"grpc": &mockChannel{}},
		workers:       make(map[string]*channelWorker),
		bus:           bus.NewMessageBus(),
		config:        cfg,
		channelHashes: toChannelHashes(cfg),
	}

	if err := m.Reload(context.Background(), config.DefaultConfig()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if _, ok := m.channels["grpc"]; !ok {
		t.Error("o canal grpc sumiu no reload — a web para de receber resposta")
	}
}
