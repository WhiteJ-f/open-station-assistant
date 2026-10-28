package main

import (
	"log"
	"math"
	"os"
	"sync"
	"syscall"
	"time"
)

type LEDMode int

const (
	ModeOff LEDMode = iota
	ModeIdle
	ModeListening
	ModeThinking
	ModeSpeaking
	ModeVolume
	ModeMusic
	ModeMute
)

type LEDManager struct {
	mu              sync.Mutex
	currentMode     LEDMode
	lastMode        LEDMode
	volumeLevel     int
	voiceBrightness byte
	volumeTimer     *time.Timer
	stopChan        chan struct{}
	i2cFile         *os.File
}

func NewLEDManager() *LEDManager {
	lm := &LEDManager{
		currentMode:     ModeIdle,
		lastMode:        ModeIdle,
		volumeLevel:     15,
		voiceBrightness: 12,
		stopChan:        make(chan struct{}),
	}
	lm.initHardware()
	go lm.animationLoop()
	return lm
}

func (lm *LEDManager) initHardware() {
	fd, e := syscall.Open("/dev/i2c-0", syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if e != nil {
		log.Printf("[LED] Failed to open /dev/i2c-0: %v", e)
		return
	}
	if _, _, err := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), 0x0703, 0x3c); err != 0 {
		log.Printf("[LED] IOCTL 0x0703 to 0x3c failed: %v", err)
		_ = syscall.Close(fd)
		return
	}
	lm.i2cFile = os.NewFile(uintptr(fd), "/dev/i2c-0")

	// 1. Enable chip: reg 0x00 -> 0x01
	_ = lm.writeReg(0x00, 0x01)

	// 2. Enable output state registers for all 36 channels (regs 0x26 - 0x49 -> 0x03)
	for ch := byte(1); ch <= 36; ch++ {
		_ = lm.writeReg(0x25+ch, 0x03)
	}
	log.Println("[LED] Hardware initialized successfully on /dev/i2c-0 (addr 0x3c, 12 RGB LEDs)")
}

func (lm *LEDManager) writeReg(reg, val byte) error {
	if lm.i2cFile == nil {
		return nil
	}
	_, err := lm.i2cFile.Write([]byte{reg, val})
	return err
}

func (lm *LEDManager) SetMode(mode LEDMode) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	if lm.currentMode != ModeVolume {
		lm.lastMode = mode
	}
	lm.currentMode = mode
}

func (lm *LEDManager) SetVoiceBrightness(level byte) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	if level < 6 {
		level = 6
	}
	if level > 60 {
		level = 60
	}
	lm.voiceBrightness = level
}

func (lm *LEDManager) ShowVolume(vol int) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	lm.volumeLevel = vol
	lm.currentMode = ModeVolume

	if lm.volumeTimer != nil {
		lm.volumeTimer.Stop()
	}
	lm.volumeTimer = time.AfterFunc(1400*time.Millisecond, func() {
		lm.mu.Lock()
		defer lm.mu.Unlock()
		if lm.currentMode == ModeVolume {
			lm.currentMode = lm.lastMode
		}
	})
}

// StartupFlash briefly pulses the LED ring in vibrant purple for visual confirmation on boot.
func (lm *LEDManager) StartupFlash() {
	for step := 0; step < 14; step++ {
		phase := float64(step) / 14.0 * math.Pi
		pulse := byte(math.Sin(phase) * 55.0)
		r := byte(float64(pulse) * 0.75)
		var colors [12][3]byte
		for i := 0; i < 12; i++ {
			colors[i] = [3]byte{r, 0, pulse}
		}
		lm.writeLEDs(colors)
		time.Sleep(50 * time.Millisecond)
	}
	var off [12][3]byte
	lm.writeLEDs(off)
}

func (lm *LEDManager) writeLEDs(colors [12][3]byte) {
	if lm.i2cFile == nil {
		return
	}

	// 12 RGB LEDs = 36 channels:
	// For LED i: (i*3 + 1)=Blue, (i*3 + 2)=Green, (i*3 + 3)=Red
	for i := 0; i < 12; i++ {
		chB := byte(i*3 + 1)
		chG := byte(i*3 + 2)
		chR := byte(i*3 + 3)

		_ = lm.writeReg(chB, colors[i][2]) // Blue PWM
		_ = lm.writeReg(chG, colors[i][1]) // Green PWM
		_ = lm.writeReg(chR, colors[i][0]) // Red PWM
	}

	// Latch frame (0x4a) then commit update (0x25)
	_ = lm.writeReg(0x4a, 0x00)
	_ = lm.writeReg(0x25, 0x00)
}

// getVolumeColor returns smooth gradient color (Cyan -> Green -> Amber -> Crimson) based on volume (0..100)
func getVolumeColor(vol int) [3]byte {
	if vol < 30 {
		// 0..30%: Cyan / Deep Teal -> Green
		t := float64(vol) / 30.0
		r := byte(0)
		g := byte(30.0 + t*25.0)
		b := byte(65.0 * (1.0 - t))
		return [3]byte{r, g, b}
	} else if vol < 70 {
		// 30..70%: Green -> Amber / Warm Orange
		t := float64(vol-30) / 40.0
		r := byte(t * 60.0)
		g := byte(55.0 - t*20.0)
		b := byte(0)
		return [3]byte{r, g, b}
	} else {
		// 70..100%: Orange -> Crimson Red
		t := float64(vol-70) / 30.0
		r := byte(60.0 + t*20.0)
		g := byte(35.0 * (1.0 - t))
		b := byte(t * 8.0)
		return [3]byte{r, g, b}
	}
}

func (lm *LEDManager) animationLoop() {
	var step int
	ticker := time.NewTicker(65 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-lm.stopChan:
			return
		case <-ticker.C:
			lm.mu.Lock()
			mode := lm.currentMode
			vol := lm.volumeLevel
			voiceB := lm.voiceBrightness
			lm.mu.Unlock()

			var colors [12][3]byte
			step++

			switch mode {
			case ModeOff, ModeIdle:
				// Standby: Completely OFF (zero power)
				lm.writeLEDs(colors)

			case ModeListening:
				// Breathing purple when listening (smooth sinusoidal pulsation)
				phase := float64(step%28) / 28.0 * 2.0 * math.Pi
				intensity := (math.Sin(phase) + 1.0) / 2.0
				pulse := byte(8.0 + intensity*40.0)
				r := byte(float64(pulse) * 0.70)
				for i := 0; i < 12; i++ {
					colors[i] = [3]byte{r, 0, pulse}
				}
				lm.writeLEDs(colors)

			case ModeThinking:
				// Spinning blue when processing
				head := step % 12
				tail1 := (head + 11) % 12
				tail2 := (head + 10) % 12
				tail3 := (head + 9) % 12
				tail4 := (head + 8) % 12
				colors[head] = [3]byte{0, 30, 80}
				colors[tail1] = [3]byte{0, 15, 45}
				colors[tail2] = [3]byte{0, 6, 22}
				colors[tail3] = [3]byte{0, 2, 10}
				colors[tail4] = [3]byte{0, 0, 4}
				lm.writeLEDs(colors)

			case ModeSpeaking:
				// Dynamic voice-based purple tracking vocal waveform amplitude
				r := byte(float64(voiceB) * 0.70)
				for i := 0; i < 12; i++ {
					colors[i] = [3]byte{r, 0, voiceB}
				}
				lm.writeLEDs(colors)

			case ModeVolume:
				// Discrete 10-step / 12-LED volume arc with authentic color gradient
				numLEDs := int(math.Round(float64(vol) * 12.0 / 100.0))
				if numLEDs < 1 && vol > 0 {
					numLEDs = 1
				}
				if numLEDs > 12 {
					numLEDs = 12
				}
				col := getVolumeColor(vol)
				for i := 0; i < 12; i++ {
					if i < numLEDs {
						colors[i] = col
					}
				}
				lm.writeLEDs(colors)

			case ModeMusic:
				// Multi-color dynamic audio visualizer for music
				for i := 0; i < 12; i++ {
					phase := float64(step*3+i*2) * 0.25
					intensity := (math.Sin(phase) + 1.0) / 2.0
					b := byte(10.0 + intensity*45.0)
					switch (i + step/4) % 3 {
					case 0:
						colors[i] = [3]byte{0, byte(float64(b) * 0.7), b}
					case 1:
						colors[i] = [3]byte{b, 0, byte(float64(b) * 0.8)}
					case 2:
						colors[i] = [3]byte{b, byte(float64(b) * 0.5), 0}
					}
				}
				lm.writeLEDs(colors)

			case ModeMute:
				// Solid red when muted
				for i := 0; i < 12; i++ {
					colors[i] = [3]byte{55, 0, 0}
				}
				lm.writeLEDs(colors)
			}
		}
	}
}

func (lm *LEDManager) Close() {
	close(lm.stopChan)
	var colors [12][3]byte
	lm.writeLEDs(colors)
	if lm.i2cFile != nil {
		_ = lm.i2cFile.Close()
	}
}
