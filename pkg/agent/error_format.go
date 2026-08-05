package agent

import (
	"fmt"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func formatProcessingError(err error) string {
	if err == nil {
		return ""
	}

	// Conta sem crédito. Este texto vai INTEIRO para o usuário final pelos
	// canais de mensagem (WhatsApp/Telegram) e pelo cron, então é o único ramo
	// aqui sem o bloco "Original error:": um JSON de 429 cru num DM é pior que
	// mensagem nenhuma. Em português porque é string de UI, mesmo dentro do
	// fork — a convenção do repo separa código (inglês) de UI (português).
	//
	// O texto serve para "acabaram" e para "nunca teve" (não há concessão de
	// boas-vindas), e não promete recarga enquanto não existir fluxo de compra.
	// (seucaranguejo fork)
	if providers.IsInsufficientCreditError(err) {
		return "Sua conta está sem créditos. Veja o saldo em Configurações → Uso; " +
			"fale com o responsável pela conta para liberar."
	}

	if kind, ok := providers.ClassifyAuthError(err); ok {
		return fmt.Sprintf(
			"Error processing message: %s\n\nOriginal error:\n%s",
			authErrorFriendlyMessage(kind),
			err.Error(),
		)
	}

	return fmt.Sprintf("Error processing message: %v", err)
}

func authErrorFriendlyMessage(kind providers.AuthErrorKind) string {
	switch kind {
	case providers.AuthErrorInvalidAPIKey:
		return "Authentication failed: the API key appears to be invalid. Check the API key configured for this model or provider."
	case providers.AuthErrorMissingAPIKey:
		return "Authentication failed: no API key is configured for this model or provider. Add an API key in the model settings or config."
	case providers.AuthErrorExpiredToken:
		return "Authentication failed: the saved login or token appears to be expired. Re-authenticate the provider."
	default:
		return "Authentication failed: check the API key, token, OAuth login, or provider permissions for this model."
	}
}
