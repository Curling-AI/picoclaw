package agent

import (
	"context"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// SteerSessionTranscribesAudio tells the embedding server that a message
// SteerSession queues reaches the model with its audio transcribed, or with
// the note that replaces it. ethos announces it to the control plane only
// through this constant, so a pin without the transcription does not build.
// (seucaranguejo fork, MST-553)
const SteerSessionTranscribesAudio = true

// transcribeJoinedAudio gives a message SteerSession marked what a message
// that opens a turn gets in prepareInboundMessageForAgent: each audio
// transcribed, or the note that replaces it. It runs where the turn reads the
// message, not where SteerSession queues it: ethos queues under the pod's chat
// lock, which the stop button and ChatActive also take, and a speech-to-text
// call there would hold them. The caller keeps msg as it was: the screen places
// the message as the person sent it. (seucaranguejo fork, MST-553)
func (al *AgentLoop) transcribeJoinedAudio(
	ctx context.Context,
	ts *turnState,
	msg providers.Message,
) providers.Message {
	if !msg.TranscribeAudio {
		return msg
	}
	heard := al.transcribeAudioInMessage(ctx, bus.InboundMessage{
		Channel: ts.channel,
		ChatID:  ts.chatID,
		Content: msg.Content,
		Media:   msg.Media,
	})
	msg.Content, msg.Media, msg.TranscribeAudio = heard.Content, heard.Media, false
	return msg
}
