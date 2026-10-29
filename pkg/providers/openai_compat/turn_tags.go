package openai_compat

import (
	"context"
	"maps"
	"strings"

	"github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
)

const (
	turnTagPrefix       = "turn_id:"
	maxGatewayTags      = 10
	maxGatewayTagLength = 64
)

func (p *Provider) tagTurnRequest(ctx context.Context, body map[string]any) map[string]any {
	turnID := protocoltypes.TurnIDFromContext(ctx)
	tag := turnTagPrefix + turnID
	if turnID == "" || len(tag) > maxGatewayTagLength ||
		!isControlPlaneLLMProxyEndpoint(p.apiBase) || !strings.HasSuffix(p.apiBase, "/hulk") {
		return body
	}
	// extra_body is shared by concurrent requests; clone each map before adding this turn.
	out := maps.Clone(body)
	configuredOptions, _ := body["providerOptions"].(map[string]any)
	options := make(map[string]any)
	maps.Copy(options, configuredOptions)
	configuredGateway, _ := options["gateway"].(map[string]any)
	gateway := make(map[string]any)
	maps.Copy(gateway, configuredGateway)
	var tags []any
	switch configured := gateway["tags"].(type) {
	case nil:
	case []string:
		for _, value := range configured {
			tags = append(tags, value)
		}
	case []any:
		tags = append(tags, configured...)
	default:
		return body
	}
	if len(tags) >= maxGatewayTags {
		return body
	}
	gateway["tags"] = append(tags, tag)
	options["gateway"] = gateway
	out["providerOptions"] = options
	return out
}
