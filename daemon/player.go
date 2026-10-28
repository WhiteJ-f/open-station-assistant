package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
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
	clickActive  int32
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
	if vol < 5 {
		vol = 5
	}
	if vol > 100 {
		vol = 100
	}
	ap.volume = vol

	// Safe analog master floor mapping for CS8571E hardware amplifier
	var val int
	if vol <= 30 {
		val = 1
	} else if vol <= 70 {
		val = 2
	} else {
		val = 3
	}
	ap.mu.Unlock()

	// Update hardware mixer in background or quickly without blocking ToF
	go func(targetVal, currentVol int) {
		_ = exec.Command("/bin/tinymix", "-D", "0", "set", "177", fmt.Sprintf("%d", targetVal), fmt.Sprintf("%d", targetVal)).Run()
		// Pre-scale sound effects asynchronously so disk I/O never causes UI lag
		ap.rescaleWAV(ap.chimePath, ap.scaledChime, currentVol)
		ap.rescaleWAV(ap.cancelPath, ap.scaledCancel, currentVol)
		ap.rescaleWAV(ap.volumePath, ap.scaledVolume, currentVol)
	}(val, vol)
}

func (ap *AudioPlayer) rescaleWAV(srcPath, dstPath string, vol int) {
	data, err := os.ReadFile(srcPath)
	if err != nil || len(data) <= 44 {
		return
	}

	scaled := make([]byte, len(data))
	copy(scaled, data)

	dataIdx := bytes.Index(scaled, []byte("data"))
	if dataIdx == -1 || dataIdx+8 >= len(scaled) {
		return
	}
	pcmStart := dataIdx + 8

	scale := float64(vol) / 100.0
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
	_ = exec.Command("/bin/tinymix", "-D", "0", "set", "176", "1").Run()
	log.Println("[Player] Hardware speaker muted (CS8571E shutdown)")
}

func (ap *AudioPlayer) Unmute() {
	ap.mu.Lock()
	defer ap.mu.Unlock()
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

func (ap *AudioPlayer) PlayWithVU(filePath string, onFrameVU func(byte)) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return ap.Play(filePath)
	}

	var pcmSamples []int16
	dataIdx := bytes.Index(data, []byte("data"))
	if dataIdx != -1 && dataIdx+8 < len(data) {
		pcmBytes := data[dataIdx+8:]
		numSamples := len(pcmBytes) / 2
		pcmSamples = make([]int16, numSamples)
		for i := 0; i < numSamples; i++ {
			pcmSamples[i] = int16(binary.LittleEndian.Uint16(pcmBytes[i*2 : i*2+2]))
		}
	}

	frameSize := 4800
	if len(data) >= 24 && binary.LittleEndian.Uint16(data[22:24]) == 1 {
		frameSize = 2400
	}

	var vuLevels []byte
	if len(pcmSamples) > 0 {
		for i := 0; i < len(pcmSamples); i += frameSize {
			end := i + frameSize
			if end > len(pcmSamples) {
				end = len(pcmSamples)
			}
			chunk := pcmSamples[i:end]
			var sumSq float64
			for _, s := range chunk {
				sumSq += float64(s) * float64(s)
			}
			rms := math.Sqrt(sumSq / float64(len(chunk)))
			level := byte(6.0 + (rms/2200.0)*46.0)
			if level > 52 {
				level = 52
			}
			if level < 6 {
				level = 6
			}
			vuLevels = append(vuLevels, level)
		}
	}

	doneChan := make(chan struct{})
	if len(vuLevels) > 0 && onFrameVU != nil {
		go func() {
			ticker := time.NewTicker(50 * time.Millisecond)
			defer ticker.Stop()
			for _, lvl := range vuLevels {
				select {
				case <-doneChan:
					return
				case <-ticker.C:
					onFrameVU(lvl)
				}
			}
		}()
	}

	playErr := ap.Play(filePath)
	close(doneChan)
	return playErr
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

// PlayVolumeClick plays a crisp, single-flight click on volume changes without blocking ToF.
func (ap *AudioPlayer) PlayVolumeClick() {
	if !atomic.CompareAndSwapInt32(&ap.clickActive, 0, 1) {
		return // Another click is actively playing; skip to avoid ALSA contention
	}

	go func() {
		defer atomic.StoreInt32(&ap.clickActive, 0)
		target := ap.scaledVolume
		if _, err := os.Stat(target); err != nil {
			target = ap.volumePath
		}
		if _, err := os.Stat(target); err == nil {
			_ = exec.Command("/bin/tinyplay", target, "-D", "0", "-d", "0").Run()
		}
	}()
}
