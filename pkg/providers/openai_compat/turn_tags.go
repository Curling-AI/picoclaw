package openai_compat

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
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
	if turnID == "" || len(tag) > maxGatewayTagLength || !p.goesToHulk() {
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

// goesToHulk reports whether calls reach hulk through the control plane, the
// only route that reads hulk's turn tags and task header.
func (p *Provider) goesToHulk() bool {
	return isControlPlaneLLMProxyEndpoint(p.apiBase) && strings.HasSuffix(p.apiBase, "/hulk")
}

// taskIDHeader groups every call of one task in hulk (X-Hulk-Task-ID in its
// docs; header names are case-insensitive). taskLabelsHeader classifies the
// call, and hulk takes it only next to a task id.
const (
	taskIDHeader     = "X-Hulk-Task-Id"
	taskLabelsHeader = "X-Hulk-Task-Labels"
)

func (p *Provider) applyTaskIDHeader(req *http.Request) {
	taskID, labels := protocoltypes.TaskCallFromContext(req.Context())
	if taskID == "" || !p.goesToHulk() {
		return
	}
	req.Header.Set(taskIDHeader, taskID)
	if len(labels) > 0 {
		encoded, _ := json.Marshal(labels)
		req.Header.Set(taskLabelsHeader, string(encoded))
	}
}
