// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/media"
	"github.com/sipeed/picoclaw/pkg/utils"
)

// Notes that replace an audio the agent could not turn into text. Without one
// the model sees only a file path and improvises — it once shipped a user's
// audio to Google's public speech endpoint and to Hugging Face Spaces. A note
// must not match audioPlaceholderRegex, or injectPathTags overwrites it with the
// path tag, and must not contain "]". They only restrict getting at the audio's
// content: IsAudioFile also matches plain .mp3/.ogg attachments, which the user
// may want saved to Drive or emailed as-is. The last sentence follows the
// AGENTS.md rule ethos generates. (seucaranguejo fork)
const (
	untranscribedAudioRule = "Do not transcribe it yourself or send it to a service to transcribe it, " +
		"unless the user asked for that service or it is one of your configured connectors.]"
	noTranscriberAudioNote = "[audio not transcribed: this assistant has no speech-to-text. " +
		"If the user needs what it says, tell them you can't listen to it and ask for the content as text. " +
		untranscribedAudioRule
	failedTranscriptionAudioNote = "[audio not transcribed: transcription failed this time. " +
		"If the user needs what it says, ask them to resend it or send the content as text. " +
		untranscribedAudioRule
	noSpeechAudioNote = "[audio not transcribed: no speech was detected in it. " +
		"If the user expected words in it, tell them none were detected. " +
		untranscribedAudioRule
)

func (al *AgentLoop) transcribeAudioInMessage(ctx context.Context, msg bus.InboundMessage) bus.InboundMessage {
	if al.mediaStore == nil || len(msg.Media) == 0 {
		return msg
	}

	// Transcribe each audio media ref in order.
	var transcriptions []string
	var replacements []string
	var keptMedia []string
	for _, ref := range msg.Media {
		path, meta, err := al.resolveAudioRef(ref)
		if err != nil {
			logger.WarnCF("voice", "Failed to resolve media ref", map[string]any{"ref": ref, "error": err})
			keptMedia = append(keptMedia, ref)
			continue
		}
		if !utils.IsAudioFile(meta.Filename, meta.ContentType) {
			keptMedia = append(keptMedia, ref)
			continue
		}
		text, note := al.transcribeAudio(ctx, ref, path)
		transcriptions = append(transcriptions, text)
		if note != "" {
			replacements = append(replacements, note)
			keptMedia = append(keptMedia, ref)
			continue
		}
		replacements = append(replacements, "[voice: "+text+"]")
	}

	if len(transcriptions) == 0 {
		return msg
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
	return msg
}

// resolveAudioRef also accepts a plain workspace path: the web channel (gRPC)
// and the pod /message route attach uploads that way, not as media:// refs.
// Like resolveMediaRefs, it sniffs a missing content type (web uploads, Telegram
// documents) so .opus/.oga count as audio. (seucaranguejo fork)
func (al *AgentLoop) resolveAudioRef(ref string) (string, media.MediaMeta, error) {
	path, meta := ref, media.MediaMeta{Filename: filepath.Base(ref)}
	if !isLocalMediaPath(ref) {
		var err error
		if path, meta, err = al.mediaStore.ResolveWithMeta(ref); err != nil {
			return "", meta, err
		}
	}
	meta.ContentType = detectMIME(path, meta)
	return path, meta, nil
}

// transcribeAudio returns the text, or the note that replaces the audio when
// there is none: no transcriber, a failed call, or no speech in the audio.
func (al *AgentLoop) transcribeAudio(ctx context.Context, ref, path string) (text, note string) {
	if al.transcriber == nil {
		return "", noTranscriberAudioNote
	}
	result, err := al.transcriber.Transcribe(ctx, path)
	if err != nil {
		logger.WarnCF("voice", "Transcription failed", map[string]any{"ref": ref, "error": err})
		return "", failedTranscriptionAudioNote
	}
	if result.Text == "" {
		return "", noSpeechAudioNote
	}
	return result.Text, ""
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
