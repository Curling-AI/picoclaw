package protocoltypes

// Hulk task labels (X-Hulk-Task-Labels) form one closed vocabulary, so the
// gateway's filters read the same in every product. A task: label says what
// started the task and rides on every call of it; a role: label says what one
// call does within it.
const (
	TaskMessage       = "task:message"
	TaskAutomation    = "task:automation"
	TaskMemoryRefresh = "task:memory-refresh"
	TaskEvolution     = "task:evolution"
	TaskDelegation    = "task:delegation"
	TaskTitle         = "task:title"
	TaskSuggestions   = "task:suggestions"
	TaskReaction      = "task:reaction"

	RoleAgent      = "role:agent"
	RoleSubagent   = "role:subagent"
	RoleCompaction = "role:compaction"
	RoleVision     = "role:vision"
	RoleImage      = "role:image"
)

// channelLabels names the channels a message can arrive on; "grpc" is the web chat.
var channelLabels = map[string]string{
	"grpc":     "channel:web",
	"whatsapp": "channel:whatsapp",
	"telegram": "channel:telegram",
	"slack":    "channel:slack",
	"discord":  "channel:discord",
}

var taskLabelVocabulary = func() map[string]bool {
	set := map[string]bool{}
	for _, label := range []string{
		TaskMessage, TaskAutomation, TaskMemoryRefresh, TaskEvolution, TaskDelegation,
		TaskTitle, TaskSuggestions, TaskReaction,
		RoleAgent, RoleSubagent, RoleCompaction, RoleVision, RoleImage,
	} {
		set[label] = true
	}
	for _, label := range channelLabels {
		set[label] = true
	}
	return set
}()

// MessageTaskLabels labels a task a message started on channel; a channel
// outside the vocabulary goes without a channel label.
func MessageTaskLabels(channel string) []string {
	if label, ok := channelLabels[channel]; ok {
		return []string{TaskMessage, label}
	}
	return []string{TaskMessage}
}

// IsTaskLabel reports whether label belongs to the vocabulary.
func IsTaskLabel(label string) bool {
	return taskLabelVocabulary[label]
}
