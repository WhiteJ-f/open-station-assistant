package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
)

type AudioPlayer struct {
	mu           sync.Mutex
	currentCmd   *exec.Cmd
	volume       int
	chimePath    string
	cancelPath   string
	volumePath   string
	scaledChime  string
	scaledCancel string
	scaledVolume string
}

func NewAudioPlayer(cfg *Config) *AudioPlayer {
	ap := &AudioPlayer{
		volume:       cfg.DefaultVolume,
		chimePath:    cfg.ChimeSound,
		cancelPath:   cfg.CancelSound,
		volumePath:   cfg.VolumeSound,
		scaledChime:  "/tmp/chime_scaled.wav",
		scaledCancel: "/tmp/cancel_scaled.wav",
		scaledVolume: "/tmp/volume_scaled.wav",
	}
	// Enable CS8571E amplifier (tinymix 176 0 -> gpio-413 out hi -> AMPLIFIER POWER ON)
	_ = exec.Command("/bin/tinymix", "-D", "0", "set", "176", "0").Run()
	ap.SetVolume(cfg.DefaultVolume)
	return ap
}

func (ap *AudioPlayer) SetVolume(vol int) {
	ap.mu.Lock()
	defer ap.mu.Unlock()
	if vol < 0 {
		vol = 0
	}
	if vol > 100 {
		vol = 100
	}
	ap.volume = vol

	// Safe analog floor mapping for CS8571E amplifier
	var val int
	if vol == 0 {
		val = 0
	} else if vol <= 30 {
		val = 1
	} else if vol <= 70 {
		val = 2
	} else {
		val = 3
	}

	_ = exec.Command("/bin/tinymix", "-D", "0", "set", "177", fmt.Sprintf("%d", val), fmt.Sprintf("%d", val)).Run()
	log.Printf("[Player] Volume set to %d%% (Master ALSA: %d/32)", ap.volume, val)

	// Pre-scale all sound effects in memory to match current volume level exactly
	ap.rescaleWAV(ap.chimePath, ap.scaledChime, vol)
	ap.rescaleWAV(ap.cancelPath, ap.scaledCancel, vol)
	ap.rescaleWAV(ap.volumePath, ap.scaledVolume, vol)
}

func (ap *AudioPlayer) rescaleWAV(srcPath, dstPath string, vol int) {
	data, err := os.ReadFile(srcPath)
	if err != nil || len(data) <= 44 {
		return
	}

	scaled := make([]byte, len(data))
	copy(scaled, data)

	// Dynamically locate the data chunk
	dataIdx := bytes.Index(scaled, []byte("data"))
	if dataIdx == -1 || dataIdx+8 >= len(scaled) {
		return
	}
	pcmStart := dataIdx + 8

	scale := float64(vol) / 100.0
	// Scale 16-bit PCM samples
	for i := pcmStart; i+1 < len(scaled); i += 2 {
		sample := int16(binary.LittleEndian.Uint16(scaled[i : i+2]))
		newSample := int16(float64(sample) * scale)
		binary.LittleEndian.PutUint16(scaled[i:i+2], uint16(newSample))
	}

	_ = os.WriteFile(dstPath, scaled, 0644)
}

func (ap *AudioPlayer) GetVolume() int {
	ap.mu.Lock()
	defer ap.mu.Unlock()
	return ap.volume
}

func (ap *AudioPlayer) Mute() {
	ap.mu.Lock()
	defer ap.mu.Unlock()
	// Shutdown amplifier (tinymix 176 1 -> gpio-413 out lo -> AMPLIFIER SHUTDOWN)
	_ = exec.Command("/bin/tinymix", "-D", "0", "set", "176", "1").Run()
	log.Println("[Player] Hardware speaker muted (CS8571E shutdown)")
}

func (ap *AudioPlayer) Unmute() {
	ap.mu.Lock()
	defer ap.mu.Unlock()
	// Enable amplifier (tinymix 176 0 -> gpio-413 out hi -> AMPLIFIER POWER ON)
	_ = exec.Command("/bin/tinymix", "-D", "0", "set", "176", "0").Run()
	log.Println("[Player] Hardware speaker unmuted (CS8571E enabled)")
}

func (ap *AudioPlayer) Stop() {
	ap.mu.Lock()
	defer ap.mu.Unlock()
	if ap.currentCmd != nil && ap.currentCmd.Process != nil {
		_ = ap.currentCmd.Process.Kill()
		ap.currentCmd = nil
		log.Println("[Player] Playback stopped")
	}
}

func (ap *AudioPlayer) Play(filePath string) error {
	ap.Stop()

	if _, err := os.Stat(filePath); err != nil {
		return fmt.Errorf("file not found: %s", filePath)
	}

	ap.mu.Lock()
	// Ensure CS8571E amplifier is ENABLED (tinymix 176 0 -> gpio-413 out hi)
	_ = exec.Command("/bin/tinymix", "-D", "0", "set", "176", "0").Run()

	cmd := exec.Command("/bin/tinyplay", filePath, "-D", "0", "-d", "0")
	ap.currentCmd = cmd
	ap.mu.Unlock()

	err := cmd.Run()

	ap.mu.Lock()
	if ap.currentCmd == cmd {
		ap.currentCmd = nil
	}
	ap.mu.Unlock()

	return err
}

func (ap *AudioPlayer) PlayChime() {
	target := ap.scaledChime
	if _, err := os.Stat(target); err != nil {
		target = ap.chimePath
	}
	if _, err := os.Stat(target); err == nil {
		_ = ap.Play(target)
	}
}

func (ap *AudioPlayer) PlayCancel() {
	target := ap.scaledCancel
	if _, err := os.Stat(target); err != nil {
		target = ap.cancelPath
	}
	if _, err := os.Stat(target); err == nil {
		_ = ap.Play(target)
	}
}

func (ap *AudioPlayer) PlayVolumeClick() {
	target := ap.scaledVolume
	if _, err := os.Stat(target); err != nil {
		target = ap.volumePath
	}
	if _, err := os.Stat(target); err == nil {
		_ = ap.Play(target)
	}
}
