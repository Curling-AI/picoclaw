package messageutil

import (
	"regexp"
	"strings"
)

const LegacyPublicProjectionPolicyID = "ethos-public-history-v1"

type LegacyPublicExclusionReason string

const (
	LegacyPublicUnsupportedRole  LegacyPublicExclusionReason = "unsupported_role"
	LegacyPublicInternalSteering LegacyPublicExclusionReason = "internal_steering"
)

type LegacyPublicProjection struct {
	Content      string
	SenderTagged bool
	Included     bool
	Reason       LegacyPublicExclusionReason
}

var legacySenderTagRE = regexp.MustCompile(`(?s)^\s*\[SENDER\b[^\]]*\]\s*\n?`)

// StripSenderTag removes the existing trusted sender prefix. Classification
// must run on raw content before this transformation.
func StripSenderTag(content string) string { return legacySenderTagRE.ReplaceAllString(content, "") }

// IsInternalSteering is the existing untagged runtime-note predicate. In
// particular, the public runtime-card prefix "[System: " is not steering.
func IsInternalSteering(content string) bool {
	content = strings.TrimSpace(content)
	return strings.HasPrefix(content, "[System]") || strings.HasPrefix(content, "[SYSTEM]")
}

// ProjectLegacyPublic preserves the existing public-session projection.
// senderTagged may preserve trusted provenance of an already projected value;
// classifiers over a producer's raw preimage must pass false and derive it
// from that preimage instead of accepting a mark from a remote consumer.
func ProjectLegacyPublic(role, content string, senderTagged bool) LegacyPublicProjection {
	if role != "user" && role != "assistant" && role != "tool" {
		return LegacyPublicProjection{Reason: LegacyPublicUnsupportedRole}
	}
	tagged := role == "user" && (senderTagged || legacySenderTagRE.MatchString(content))
	if !tagged && IsInternalSteering(content) {
		return LegacyPublicProjection{Reason: LegacyPublicInternalSteering}
	}
	if !senderTagged {
		content = StripSenderTag(content)
	}
	return LegacyPublicProjection{Content: content, SenderTagged: tagged, Included: true}
}
