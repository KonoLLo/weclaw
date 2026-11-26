package messaging

import (
	"os"
	"path/filepath"
	"time"
)

func lastVoiceDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "weclaw-tts")
	}
	return filepath.Join(home, ".weclaw", "tts")
}

// SaveLastVoice keeps the most recent clip so "把音频文件发我" can resend it.
func SaveLastVoice(mp3 []byte, text string) (string, error) {
	dir := lastVoiceDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "voice.mp3")
	if err := os.WriteFile(path, mp3, 0o644); err != nil {
		return "", err
	}
	_ = os.WriteFile(filepath.Join(dir, "voice.txt"), []byte(time.Now().Format(time.RFC3339)+"\n"+text+"\n"), 0o644)
	return path, nil
}

func LastVoicePath() string {
	return filepath.Join(lastVoiceDir(), "voice.mp3")
}
