package routing

import (
	"fmt"
	"strings"
)

// DMScope controls DM session isolation granularity.
type DMScope string

const (
	DMScopeMain                  DMScope = "main"
	DMScopePerPeer               DMScope = "per-peer"
	DMScopePerChannelPeer        DMScope = "per-channel-peer"
	DMScopePerAccountChannelPeer DMScope = "per-account-channel-peer"
)

// RoutePeer represents a chat peer with kind and ID.
type RoutePeer struct {
	Kind string // "direct", "group", "channel"
	ID   string
}

// SessionKeyParams holds all inputs for session key construction.
type SessionKeyParams struct {
	AgentID       string
	Channel       string
	AccountID     string
	Peer          *RoutePeer
	DMScope       DMScope
	IdentityLinks map[string][]string
	ThreadScope   string // "", "per-thread"
	ThreadID      string // thread timestamp or ID
}

// ParsedSessionKey is the result of parsing an agent-scoped session key.
type ParsedSessionKey struct {
	AgentID string
	Rest    string
}

// BuildAgentMainSessionKey returns "agent:<agentId>:main".
func BuildAgentMainSessionKey(agentID string) string {
	return fmt.Sprintf("agent:%s:%s", NormalizeAgentID(agentID), DefaultMainKey)
}

// BuildAgentPeerSessionKey constructs a session key based on agent, channel, peer, and DM scope.
func BuildAgentPeerSessionKey(params SessionKeyParams) string {
	agentID := NormalizeAgentID(params.AgentID)

	peer := params.Peer
	if peer == nil {
		peer = &RoutePeer{Kind: "direct"}
	}
	peerKind := strings.TrimSpace(peer.Kind)
	if peerKind == "" {
		peerKind = "direct"
	}

	if peerKind == "direct" {
		dmScope := params.DMScope
		if dmScope == "" {
			dmScope = DMScopeMain
		}
		peerID := strings.TrimSpace(peer.ID)

		// Resolve identity links (cross-platform collapse)
		if dmScope != DMScopeMain && peerID != "" {
			if linked := resolveLinkedPeerID(params.IdentityLinks, params.Channel, peerID); linked != "" {
				peerID = linked
			}
		}
		peerID = strings.ToLower(peerID)

		switch dmScope {
		case DMScopePerAccountChannelPeer:
			if peerID != "" {
				channel := normalizeChannel(params.Channel)
				accountID := NormalizeAccountID(params.AccountID)
				key := fmt.Sprintf("agent:%s:%s:%s:direct:%s", agentID, channel, accountID, peerID)
				return appendThreadScope(key, params.ThreadScope, params.ThreadID)
			}
		case DMScopePerChannelPeer:
			if peerID != "" {
				channel := normalizeChannel(params.Channel)
				key := fmt.Sprintf("agent:%s:%s:direct:%s", agentID, channel, peerID)
				return appendThreadScope(key, params.ThreadScope, params.ThreadID)
			}
		case DMScopePerPeer:
			if peerID != "" {
				key := fmt.Sprintf("agent:%s:direct:%s", agentID, peerID)
				return appendThreadScope(key, params.ThreadScope, params.ThreadID)
			}
		}
		return BuildAgentMainSessionKey(agentID)
	}

	// Group/channel peers always get per-peer sessions
	channel := normalizeChannel(params.Channel)
	peerID := strings.ToLower(strings.TrimSpace(peer.ID))
	if peerID == "" {
		peerID = "unknown"
	}
	key := fmt.Sprintf("agent:%s:%s:%s:%s", agentID, channel, peerKind, peerID)
	return appendThreadScope(key, params.ThreadScope, params.ThreadID)
}

// appendThreadScope appends a thread segment to a session key if per-thread scoping is enabled.
func appendThreadScope(key, threadScope, threadID string) string {
	if threadScope == "per-thread" && strings.TrimSpace(threadID) != "" {
		return fmt.Sprintf("%s:thread:%s", key, strings.TrimSpace(threadID))
	}
	return key
}

// ParseAgentSessionKey extracts agentId and rest from "agent:<agentId>:<rest>".
func ParseAgentSessionKey(sessionKey string) *ParsedSessionKey {
	raw := strings.TrimSpace(sessionKey)
	if raw == "" {
		return nil
	}
	parts := strings.SplitN(raw, ":", 3)
	if len(parts) < 3 {
		return nil
	}
	if parts[0] != "agent" {
		return nil
	}
	agentID := strings.TrimSpace(parts[1])
	rest := parts[2]
	if agentID == "" || rest == "" {
		return nil
	}
	return &ParsedSessionKey{AgentID: agentID, Rest: rest}
}

// IsSubagentSessionKey returns true if the session key represents a subagent.
func IsSubagentSessionKey(sessionKey string) bool {
	raw := strings.TrimSpace(sessionKey)
	if raw == "" {
		return false
	}
	if strings.HasPrefix(strings.ToLower(raw), "subagent:") {
		return true
	}
	parsed := ParseAgentSessionKey(raw)
	if parsed == nil {
		return false
	}
	return strings.HasPrefix(strings.ToLower(parsed.Rest), "subagent:")
}

func normalizeChannel(channel string) string {
	c := strings.TrimSpace(strings.ToLower(channel))
	if c == "" {
		return "unknown"
	}
	return c
}

func resolveLinkedPeerID(identityLinks map[string][]string, channel, peerID string) string {
	if len(identityLinks) == 0 {
		return ""
	}
	peerID = strings.TrimSpace(peerID)
	if peerID == "" {
		return ""
	}

	candidates := make(map[string]bool)
	rawCandidate := strings.ToLower(peerID)
	if rawCandidate != "" {
		candidates[rawCandidate] = true
	}
	channel = strings.ToLower(strings.TrimSpace(channel))
	if channel != "" {
		scopedCandidate := fmt.Sprintf("%s:%s", channel, strings.ToLower(peerID))
		candidates[scopedCandidate] = true
	}
	if len(candidates) == 0 {
		return ""
	}

	for canonical, ids := range identityLinks {
		canonicalName := strings.TrimSpace(canonical)
		if canonicalName == "" {
			continue
		}
		for _, id := range ids {
			normalized := strings.ToLower(strings.TrimSpace(id))
			if normalized != "" && candidates[normalized] {
				return canonicalName
			}
		}
	}
	return ""
}
