package telegram

import (
	"context"

	"github.com/LucianoR23/valuo_api/internal/expenseparse"
)

// MaxVoiceSeconds is the longest voice note read (plan §8.3, as in the app).
const MaxVoiceSeconds = 30

// file is the voice note or image of a message, ready to download.
type file struct {
	id         string
	mimeType   string
	audio      bool
	max        int64
	tooBig     bool
	tooBigText string
}

// pickFile chooses what to read from a message: a voice note or audio, the
// largest photo size under the image cap, or an image sent as a file.
// ok is false for anything else (a PDF, a sticker, a video).
func pickFile(m *Message) (file, bool) {
	if v := m.Voice; v != nil || m.Audio != nil {
		if v == nil {
			v = m.Audio
		}
		mt := v.MimeType
		if mt == "" {
			mt = "audio/ogg" // Telegram voice notes are Opus in Ogg
		}
		if _, ok := expenseparse.AudioType(mt); !ok {
			return file{}, false
		}
		return file{id: v.FileID, mimeType: mt, audio: true, max: expenseparse.MaxAudioBytes,
			tooBig: v.Duration > MaxVoiceSeconds || v.FileSize > expenseparse.MaxAudioBytes, tooBigText: textVoiceTooLong}, true
	}
	if len(m.Photo) > 0 {
		var best *PhotoSize
		for i := range m.Photo {
			p := &m.Photo[i]
			if p.FileSize > expenseparse.MaxImageBytes {
				continue
			}
			if best == nil || p.Width*p.Height > best.Width*best.Height {
				best = p
			}
		}
		if best == nil {
			return file{tooBig: true, tooBigText: textImageTooBig}, true
		}
		return file{id: best.FileID, mimeType: "image/jpeg", max: expenseparse.MaxImageBytes}, true
	}
	if d := m.Document; d != nil {
		if mt, ok := expenseparse.ImageType(d.MimeType); ok {
			return file{id: d.FileID, mimeType: mt, max: expenseparse.MaxImageBytes,
				tooBig: d.FileSize > expenseparse.MaxImageBytes, tooBigText: textImageTooBig}, true
		}
	}
	return file{}, false
}

func (s *Service) download(ctx context.Context, f file) ([]byte, error) {
	path, err := s.Bot.GetFile(ctx, f.id)
	if err != nil {
		return nil, err
	}
	return s.Bot.Download(ctx, path, f.max)
}

func heardLine(d draft) string {
	if d.Heard == "" {
		return ""
	}
	return d.Heard + "\n\n"
}
