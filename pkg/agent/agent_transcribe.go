// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"context"
	"strings"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/utils"
)

// untranscribedAudioNote replaces an audio the agent could not turn into text.
// Without it the model sees only a file path and improvises — it once shipped a
// user's audio to Google's public speech endpoint and to Hugging Face Spaces
// (MST-329). The tag must not match audioPlaceholderRegex, or injectPathTags
// overwrites it with the path tag, and must not contain "]". It only restricts
// getting at the audio's content: IsAudioFile also matches plain .mp3/.ogg
// attachments, which the user may want saved to Drive or emailed as-is.
const untranscribedAudioNote = "[audio not transcribed: this assistant has no way to turn this audio into text. " +
	"If the user needs what it says, tell them you can't listen to it and ask for the content as text. " +
	"Do not transcribe it yourself or send it to any service to transcribe it.]"

func (al *AgentLoop) transcribeAudioInMessage(ctx context.Context, msg bus.InboundMessage) (bus.InboundMessage, bool) {
	if al.mediaStore == nil || len(msg.Media) == 0 {
		return msg, false
	}

	// Transcribe each audio media ref in order.
	var transcriptions []string
	var replacements []string
	var keptMedia []string
	for _, ref := range msg.Media {
		path, meta, err := al.mediaStore.ResolveWithMeta(ref)
		if err != nil {
			logger.WarnCF("voice", "Failed to resolve media ref", map[string]any{"ref": ref, "error": err})
			keptMedia = append(keptMedia, ref)
			continue
		}
		if !utils.IsAudioFile(meta.Filename, meta.ContentType) {
			keptMedia = append(keptMedia, ref)
			continue
		}
		text := al.transcribeAudio(ctx, ref, path)
		transcriptions = append(transcriptions, text)
		if text == "" {
			replacements = append(replacements, untranscribedAudioNote)
			keptMedia = append(keptMedia, ref)
			continue
		}
		replacements = append(replacements, "[voice: "+text+"]")
	}

	if len(transcriptions) == 0 {
		return msg, false
	}

	if al.transcriber != nil {
		al.sendTranscriptionFeedback(ctx, msg.Channel, msg.ChatID, msg.MessageID, transcriptions)
	}

	// Replace audio annotations sequentially with transcriptions.
	idx := 0
	newContent := audioAnnotationRe.ReplaceAllStringFunc(msg.Content, func(match string) string {
		if idx >= len(replacements) {
			return match
		}
		r := replacements[idx]
		idx++
		return r
	})

	// Append any remaining transcriptions not matched by an annotation.
	for ; idx < len(replacements); idx++ {
		newContent += "\n" + replacements[idx]
	}

	msg.Content = newContent
	msg.Media = keptMedia
	return msg, true
}

// transcribeAudio returns "" when there is no transcriber, it fails, or the
// audio has no speech — all cases the agent must not try to work around.
func (al *AgentLoop) transcribeAudio(ctx context.Context, ref, path string) string {
	if al.transcriber == nil {
		return ""
	}
	result, err := al.transcriber.Transcribe(ctx, path)
	if err != nil {
		logger.WarnCF("voice", "Transcription failed", map[string]any{"ref": ref, "error": err})
		return ""
	}
	return result.Text
}

func (al *AgentLoop) sendTranscriptionFeedback(
	ctx context.Context,
	channel, chatID, messageID string,
	validTexts []string,
) {
	if !al.cfg.Voice.EchoTranscription {
		return
	}
	if al.channelManager == nil {
		return
	}

	var nonEmpty []string
	for _, t := range validTexts {
		if t != "" {
			nonEmpty = append(nonEmpty, t)
		}
	}

	var feedbackMsg string
	if len(nonEmpty) > 0 {
		feedbackMsg = "Transcript: " + strings.Join(nonEmpty, "\n")
	} else {
		feedbackMsg = "No voice detected in the audio"
	}

	err := al.channelManager.SendMessage(ctx, bus.OutboundMessage{
		Context:          bus.NewOutboundContext(channel, chatID, messageID),
		Content:          feedbackMsg,
		ReplyToMessageID: messageID,
	})
	if err != nil {
		logger.WarnCF("voice", "Failed to send transcription feedback", map[string]any{"error": err.Error()})
	}
}
