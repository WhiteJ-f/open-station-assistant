package main

import (
	"fmt"
	"log"
	"strings"
)

type MusicService struct {
	player *AudioPlayer
	leds   *LEDManager
}

func NewMusicService(player *AudioPlayer, leds *LEDManager) *MusicService {
	return &MusicService{player: player, leds: leds}
}

// PlayQuery searches and plays music based on user query.
func (ms *MusicService) PlayQuery(query string) error {
	log.Printf("[Music] Request to play: %s", query)
	ms.leds.SetMode(ModeSpeaking)

	// Built-in curated station streams & samples
	lower := strings.ToLower(query)
	var streamURL string

	switch {
	case strings.Contains(lower, "рок") || strings.Contains(lower, "rock"):
		streamURL = "http://nashe.streamr.ru/nashe-128.mp3"
	case strings.Contains(lower, "лофай") || strings.Contains(lower, "lofi") || strings.Contains(lower, "спокойную"):
		streamURL = "http://stream.zeno.fm/f3wvbbqmdg8uv"
	case strings.Contains(lower, "джаз") || strings.Contains(lower, "jazz"):
		streamURL = "http://jazz.streamr.ru/jazz-128.mp3"
	case strings.Contains(lower, "new era"):
		localTrack := "/tmp/new_era.wav"
		return ms.player.Play(localTrack)
	default:
		// Default to popular music stream
		streamURL = "http://chanson.streamr.ru/chanson-128.mp3"
	}

	log.Printf("[Music] Streaming radio stream: %s", streamURL)
	// Play stream or notify
	return fmt.Errorf("direct stream URL selected: %s", streamURL)
}
