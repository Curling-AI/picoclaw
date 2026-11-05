package utils

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers/common"
)

// Áudio de voz do WhatsApp e do Telegram é Ogg Opus, e chega também como
// .opus ou .oga (MST-351).
func TestAudioFormatAcceptsOggOpusNames(t *testing.T) {
	for _, name := range []string{"voice.opus", "voice.oga", "VOICE.OGG"} {
		format, err := AudioFormat(name)
		if err != nil {
			t.Fatalf("AudioFormat(%q): %v", name, err)
		}
		if got := AudioMIMEType(format); got != "audio/ogg" {
			t.Errorf("AudioMIMEType(%q) = %q, want audio/ogg", format, got)
		}
		if !IsAudioFile(name, "") {
			t.Errorf("IsAudioFile(%q) = false", name)
		}
	}
}

// O data URL de AudioMIMEType volta ao formato que input_audio espera (MST-351).
func TestAudioMIMETypeRoundTripsThroughParseDataAudioURL(t *testing.T) {
	for _, format := range []string{"mp3", "m4a", "ogg", "wav"} {
		mediaURL := "data:" + AudioMIMEType(format) + ";base64,QQ=="
		gotFormat, gotData, ok := common.ParseDataAudioURL(mediaURL)
		if !ok || gotFormat != format || gotData != "QQ==" {
			t.Errorf("ParseDataAudioURL(%q) = (%q, %q, %v), want (%q, %q, true)",
				mediaURL, gotFormat, gotData, ok, format, "QQ==")
		}
	}
}

func TestAudioMIMEType(t *testing.T) {
	for format, want := range map[string]string{
		"mp3": "audio/mpeg", "m4a": "audio/mp4", "wav": "audio/wav",
		"aac": "audio/aac", "flac": "audio/flac", "ogg": "audio/ogg",
	} {
		if got := AudioMIMEType(format); got != want {
			t.Errorf("AudioMIMEType(%q) = %q, want %q", format, got, want)
		}
	}
}
