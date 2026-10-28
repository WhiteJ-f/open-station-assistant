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
	mu          sync.Mutex
	currentCmd  *exec.Cmd
	volume      int
	chimePath   string
	cancelPath  string
	volumePath  string
	startupPath string
	clickActive int32
}

func NewAudioPlayer(cfg *Config) *AudioPlayer {
	ap := &AudioPlayer{
		volume:     cfg.DefaultVolume,
		chimePath:  cfg.ChimeSound,
		cancelPath: cfg.CancelSound,
		volumePath:  cfg.VolumeSound,
		startupPath: cfg.StartupSound,
	}
	// Enable CS8571E amplifier & set unmuted
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

	// Map 5..100% to ALSA Master volume (range 0..32)
	// Factory mapping: 10% -> 3/32, 30% -> 9/32, 50% -> 15/32, 100% -> 30/32
	masterVal := (vol * 30) / 100
	if masterVal < 3 {
		masterVal = 3
	}
	if masterVal > 30 {
		masterVal = 30
	}
	ap.mu.Unlock()

	go func(targetVal int) {
		_ = exec.Command("/bin/tinymix", "-D", "0", "set", "176", "0").Run()
		_ = exec.Command("/bin/tinymix", "-D", "0", "set", "177", fmt.Sprintf("%d", targetVal), fmt.Sprintf("%d", targetVal)).Run()
		log.Printf("[Player] Volume set to %d%% (Master ALSA: %d/32)", vol, targetVal)
	}(masterVal)
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
	log.Println("[Player] Hardware speaker muted")
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
	if _, err := os.Stat(ap.chimePath); err == nil {
		_ = ap.Play(ap.chimePath)
	}
}

func (ap *AudioPlayer) PlayCancel() {
	if _, err := os.Stat(ap.cancelPath); err == nil {
		_ = ap.Play(ap.cancelPath)
	}
}

// PlayVolumeClick plays a crisp, single-flight click on volume changes without blocking ToF.
func (ap *AudioPlayer) PlayVolumeClick() {
	if !atomic.CompareAndSwapInt32(&ap.clickActive, 0, 1) {
		return // Another click is actively playing; skip to avoid ALSA contention
	}

	go func() {
		defer atomic.StoreInt32(&ap.clickActive, 0)
		if _, err := os.Stat(ap.volumePath); err == nil {
			_ = exec.Command("/bin/tinyplay", ap.volumePath, "-D", "0", "-d", "0").Run()
		}
	}()
}

func (ap *AudioPlayer) PlayStartup() {
	if _, err := os.Stat(ap.startupPath); err == nil {
		_ = ap.Play(ap.startupPath)
	}
}
