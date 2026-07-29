package agent

import (
	"fmt"
	"os"
	"strings"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func promptBuildRequestForTurn(
	ts *turnState,
	history []providers.Message,
	summary string,
	currentMessage string,
	media []string,
	cfg *config.Config,
) PromptBuildRequest {
	req := PromptBuildRequest{
		History:           history,
		Summary:           summary,
		CurrentMessage:    currentMessage,
		Media:             append([]string(nil), media...),
		Channel:           ts.channel,
		ChatID:            ts.chatID,
		SenderID:          ts.opts.Dispatch.SenderID(),
		SenderDisplayName: ts.opts.SenderDisplayName,
		ActiveSkills:      activeSkillNames(ts.agent, ts.opts),
		Overlays:          promptOverlaysForOptions(ts.opts),
		Loop:              ts.opts.Loop,
	}
	hasCallableTools := true
	if ts.profile.Enabled {
		hasCallableTools = turnProfileHasCallableTools(ts.profile, ts.agent.Tools.ToProviderDefs()) ||
			turnProfileNativeSearchCallable(cfg, ts.profile, ts.agent)
	}
	if turnProfileSystemPromptOff(ts.profile) {
		req.SuppressDefaultSystemPrompt = true
		req.SuppressSkillContext = true
		req.ToolUseFallback = hasCallableTools
	}
	if ts.profile.Enabled && !hasCallableTools {
		req.SuppressToolUseRule = true
	}
	if turnProfileSkillsOff(ts.profile) {
		req.SuppressSkillContext = true
	}
	if turnProfileCustomSkills(ts.profile) {
		req.AllowedSkills = append([]string(nil), ts.profile.AllowedSkills...)
	}
	if ts.profile.Enabled && ts.profile.ToolsMode == config.TurnProfileModeCustom {
		req.AllowedTools = append([]string(nil), ts.profile.AllowedTools...)
	}
	return req
}

func turnProfileNativeSearchCallable(
	cfg *config.Config,
	profile config.EffectiveTurnProfile,
	agent *AgentInstance,
) bool {
	if cfg == nil || agent == nil {
		return false
	}
	if !cfg.Tools.IsToolEnabled("web") || !cfg.Tools.Web.PreferNative {
		return false
	}
	if !turnProfileToolAllowed(profile, "web_search") {
		return false
	}
	nativeProvider, ok := agent.Provider.(providers.NativeSearchCapable)
	return ok && nativeProvider.SupportsNativeSearch()
}

func promptBuildRequestForProcessOptions(
	agent *AgentInstance,
	opts processOptions,
	history []providers.Message,
	summary string,
	currentMessage string,
	media []string,
) PromptBuildRequest {
	req := PromptBuildRequest{
		History:           history,
		Summary:           summary,
		CurrentMessage:    currentMessage,
		Media:             append([]string(nil), media...),
		Channel:           opts.Channel,
		ChatID:            opts.ChatID,
		SenderID:          opts.SenderID,
		SenderDisplayName: opts.SenderDisplayName,
		ActiveSkills:      activeSkillNames(agent, opts),
		Overlays:          promptOverlaysForOptions(opts),
		Loop:              opts.Loop,
	}
	profile := opts.TurnProfile
	hasCallableTools := true
	if profile.Enabled && agent != nil {
		hasCallableTools = turnProfileHasCallableTools(profile, agent.Tools.ToProviderDefs())
	}
	if turnProfileSystemPromptOff(profile) {
		req.SuppressDefaultSystemPrompt = true
		req.SuppressSkillContext = true
		req.ToolUseFallback = hasCallableTools
	}
	if profile.Enabled && !hasCallableTools {
		req.SuppressToolUseRule = true
	}
	if turnProfileSkillsOff(profile) {
		req.SuppressSkillContext = true
	}
	if turnProfileCustomSkills(profile) {
		req.AllowedSkills = append([]string(nil), profile.AllowedSkills...)
	}
	if profile.Enabled && profile.ToolsMode == config.TurnProfileModeCustom {
		req.AllowedTools = append([]string(nil), profile.AllowedTools...)
	}
	return req
}

func promptOverlaysForOptions(opts processOptions) []PromptPart {
	var parts []PromptPart

	// Loop: instruções e memória do loop deste turno. Entra como OVERLAY por
	// request, e nunca no prompt estático — cb.cachedSystemPrompt é UMA string
	// por pod, invalidada só por mtime e sem chave por sessão, então conteúdo de
	// loop lá dentro faria o prompt do Loop A ser servido ao Loop B. Isso é bug
	// de correção, não de performance. (seucaranguejo fork)
	if part := loopPromptPart(opts.Loop); part != nil {
		parts = append(parts, *part)
	}

	if systemPrompt := strings.TrimSpace(opts.SystemPromptOverride); systemPrompt != "" {
		parts = append(parts, PromptPart{
			ID:      "instruction.subturn_profile",
			Layer:   PromptLayerInstruction,
			Slot:    PromptSlotWorkspace,
			Source:  PromptSource{ID: PromptSourceSubTurnProfile, Name: "subturn.profile"},
			Title:   "SubTurn System Instructions",
			Content: systemPrompt,
			Stable:  false,
			Cache:   PromptCacheNone,
		})
	}

	return parts
}

// loopPromptPart monta o bloco do loop: instruções + memória própria.
//
// Vem DEPOIS do prompt estático (que traz AGENTS.md, USER.md e a memória
// global), então é camada por cima, não substituição — a decisão de produto era
// herdar o global e somar o do loop.
//
// Marcado como cacheável: o conteúdo é estável enquanto a conversa continua no
// mesmo loop, então vira um segundo breakpoint de cache em vez de texto novo a
// cada turno.
func loopPromptPart(scope LoopScope) *PromptPart {
	if !scope.Active() {
		return nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "# Loop: %s\n", scope.Slug)
	sb.WriteString("\nEsta conversa pertence a um loop — uma meta com prazo. " +
		"O que segue vale para todo trabalho feito aqui dentro.\n")

	if body := readTrimmedFile(loopInstructionsFile(scope.Root)); body != "" {
		sb.WriteString("\n" + body + "\n")
	}

	// Delimitador em vez de heading, pela mesma razão da memória global: heading
	// é contenção fraca — o modelo o trata como sugestão de seção, não como
	// fronteira de conteúdo.
	if mem := readTrimmedFile(loopMemoryFile(scope.Root)); mem != "" {
		fmt.Fprintf(&sb, "\n<memory scope=\"loop:%s\">\n%s\n</memory>\n", scope.Slug, mem)
	}

	// Catálogo das skills que ESTE loop aprendeu. Vai aqui, no overlay, e não no
	// catálogo do prompt estático: aquele é cacheado por pod e vazaria as skills
	// de um loop para todos os outros. É também o único jeito de o modelo saber
	// que elas existem — o find_installed_skills enumera só as três raízes fixas.
	if catalog := loopSkillCatalog(scope); catalog != "" {
		sb.WriteString("\n## Skills deste loop\n\n" +
			"Aprendidas aqui dentro, em ciclos anteriores. Preferem-se às globais de mesmo nome.\n\n" +
			catalog)
	}

	return &PromptPart{
		ID:      "instruction.loop",
		Layer:   PromptLayerInstruction,
		Slot:    PromptSlotWorkspace,
		Source:  PromptSource{ID: PromptSourceLoop, Name: "loop:" + scope.Slug},
		Title:   "Loop",
		Content: sb.String(),
		Stable:  true,
		Cache:   PromptCacheEphemeral,
	}
}

// readTrimmedFile devolve o conteúdo ou "" — arquivo ausente é estado normal
// (loop recém-criado, pod novo), não erro.
func readTrimmedFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func promptContentBlock(part PromptPart, cache *providers.CacheControl) providers.ContentBlock {
	if cache == nil {
		cache = cacheControlForPromptPart(part)
	}
	return providers.ContentBlock{
		Type:         "text",
		Text:         part.Content,
		CacheControl: cache,
		PromptLayer:  string(part.Layer),
		PromptSlot:   string(part.Slot),
		PromptSource: string(part.Source.ID),
	}
}

func cacheControlForPromptPart(part PromptPart) *providers.CacheControl {
	switch part.Cache {
	case PromptCacheEphemeral:
		return &providers.CacheControl{Type: "ephemeral"}
	default:
		return nil
	}
}

func promptMessageWithMetadata(
	msg providers.Message,
	layer PromptLayer,
	slot PromptSlot,
	source PromptSourceID,
) providers.Message {
	msg.PromptLayer = string(layer)
	msg.PromptSlot = string(slot)
	msg.PromptSource = string(source)
	return msg
}

func promptMessageWithDefaultMetadata(
	msg providers.Message,
	layer PromptLayer,
	slot PromptSlot,
	source PromptSourceID,
) providers.Message {
	if strings.TrimSpace(msg.PromptSource) != "" {
		return msg
	}
	return promptMessageWithMetadata(msg, layer, slot, source)
}

func userPromptMessage(content string, media []string) providers.Message {
	msg := providers.Message{
		Role:    "user",
		Content: content,
	}
	if len(media) > 0 {
		msg.Media = append([]string(nil), media...)
	}
	return promptMessageWithMetadata(msg, PromptLayerTurn, PromptSlotMessage, PromptSourceUserMessage)
}

func toolResultPromptMessage(content, toolCallID string, media []string) providers.Message {
	msg := providers.Message{
		Role:       "tool",
		Content:    content,
		ToolCallID: toolCallID,
	}
	if len(media) > 0 {
		msg.Media = append([]string(nil), media...)
	}
	return promptMessageWithMetadata(msg, PromptLayerTurn, PromptSlotToolResult, PromptSourceToolResult)
}

func toolImageFollowUpPromptMessage(media []string) providers.Message {
	msg := providers.Message{
		Role:    "user",
		Content: "[Loaded image from tool result above]",
	}
	if len(media) > 0 {
		msg.Media = append([]string(nil), media...)
	}
	return promptMessageWithMetadata(msg, PromptLayerTurn, PromptSlotToolResult, PromptSourceToolResult)
}

func steeringPromptMessage(msg providers.Message) providers.Message {
	return promptMessageWithDefaultMetadata(msg, PromptLayerTurn, PromptSlotSteering, PromptSourceSteering)
}

func subTurnResultPromptMessage(content string) providers.Message {
	return promptMessageWithMetadata(
		providers.Message{Role: "user", Content: fmt.Sprintf("[SubTurn Result] %s", content)},
		PromptLayerTurn,
		PromptSlotSubTurn,
		PromptSourceSubTurnResult,
	)
}

func interruptPromptMessage(content string) providers.Message {
	return promptMessageWithMetadata(
		providers.Message{Role: "user", Content: content},
		PromptLayerTurn,
		PromptSlotInterrupt,
		PromptSourceInterrupt,
	)
}
