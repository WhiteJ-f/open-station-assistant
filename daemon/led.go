package main

import (
	"math"
	"os"
	"sync"
	"syscall"
	"time"
)

const I2C_SLAVE_FORCE = 0x0706

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
		voiceBrightness: 6,
		stopChan:        make(chan struct{}),
	}
	lm.initHardware()
	go lm.animationLoop()
	return lm
}

func (lm *LEDManager) initHardware() {
	f, err := os.OpenFile("/dev/i2c-0", os.O_RDWR, 0)
	if err != nil {
		return
	}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), I2C_SLAVE_FORCE, uintptr(0x3c))
	if errno != 0 {
		f.Close()
		return
	}
	lm.i2cFile = f

	// Enable chip: reg 0x00 -> 0x01
	_, _ = lm.i2cFile.Write([]byte{0x00, 0x01})

	// Enable all 36 channel control registers (0x26 - 0x49)
	for ch := 1; ch <= 36; ch++ {
		reg := byte(0x25 + ch)
		_, _ = lm.i2cFile.Write([]byte{reg, 0x03})
	}
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
	if level < 4 {
		level = 4
	}
	if level > 55 {
		level = 55
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
	lm.volumeTimer = time.AfterFunc(1500*time.Millisecond, func() {
		lm.mu.Lock()
		defer lm.mu.Unlock()
		if lm.currentMode == ModeVolume {
			lm.currentMode = lm.lastMode
		}
	})
}

func (lm *LEDManager) writeLEDs(colors [12][3]byte) {
	if lm.i2cFile == nil {
		return
	}

	for i := 0; i < 12; i++ {
		chB := byte(i*3 + 1)
		chG := byte(i*3 + 2)
		chR := byte(i*3 + 3)

		_, _ = lm.i2cFile.Write([]byte{chB, colors[i][2]})
		_, _ = lm.i2cFile.Write([]byte{chG, colors[i][1]})
		_, _ = lm.i2cFile.Write([]byte{chR, colors[i][0]})
	}
	// Commit update: reg 0x25 = 0
	_, _ = lm.i2cFile.Write([]byte{0x25, 0x00})
}

// getVolumeColor returns smooth gradient color (Cyan -> Green -> Orange -> Crimson) based on volume (0..100)
func getVolumeColor(vol int) [3]byte {
	if vol < 30 {
		// 0..30%: Cyan / Deep Teal -> Green
		t := float64(vol) / 30.0
		r := byte(0)
		g := byte(25.0 + t*25.0)
		b := byte(55.0 * (1.0 - t))
		return [3]byte{r, g, b}
	} else if vol < 70 {
		// 30..70%: Green -> Amber / Warm Orange
		t := float64(vol-30) / 40.0
		r := byte(t * 50.0)
		g := byte(50.0 - t*20.0)
		b := byte(0)
		return [3]byte{r, g, b}
	} else {
		// 70..100%: Orange -> Crimson Red
		t := float64(vol-70) / 30.0
		r := byte(50.0 + t*15.0)
		g := byte(30.0 * (1.0 - t))
		b := byte(t * 6.0)
		return [3]byte{r, g, b}
	}
}

func (lm *LEDManager) animationLoop() {
	var step int
	ticker := time.NewTicker(70 * time.Millisecond)
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
				// 1. Off when standby (clean zero power / black)
				lm.writeLEDs(colors)

			case ModeListening:
				// 2. Breathing purple when listening (smooth sinusoidal pulsation)
				phase := float64(step%28) / 28.0 * 2.0 * math.Pi
				intensity := (math.Sin(phase) + 1.0) / 2.0
				pulse := byte(6.0 + intensity*34.0)
				r := byte(float64(pulse) * 0.65)
				for i := 0; i < 12; i++ {
					colors[i] = [3]byte{r, 0, pulse}
				}
				lm.writeLEDs(colors)

			case ModeThinking:
				// 3. Spinning blue when processing (fluid rotation with fading trail)
				head := step % 12
				tail1 := (head + 11) % 12
				tail2 := (head + 10) % 12
				tail3 := (head + 9) % 12
				tail4 := (head + 8) % 12
				colors[head] = [3]byte{0, 20, 65}
				colors[tail1] = [3]byte{0, 10, 35}
				colors[tail2] = [3]byte{0, 4, 18}
				colors[tail3] = [3]byte{0, 1, 8}
				colors[tail4] = [3]byte{0, 0, 3}
				lm.writeLEDs(colors)

			case ModeSpeaking:
				// 4. Dynamic voice-based purple: brightness dynamically driven by speech amplitude
				r := byte(float64(voiceB) * 0.65)
				for i := 0; i < 12; i++ {
					colors[i] = [3]byte{r, 0, voiceB}
				}
				lm.writeLEDs(colors)

			case ModeVolume:
				// 5. Original-alike volume arc with dynamic color gradient
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
				// 6. Visualizer for music playback: animated multi-color EQ wave
				for i := 0; i < 12; i++ {
					phase := float64(step*3+i*2) * 0.25
					intensity := (math.Sin(phase) + 1.0) / 2.0
					b := byte(8.0 + intensity*40.0)
					// Vibrant audio visualizer: rotates between cyan, magenta, and amber
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
				// 7. Red when muted
				for i := 0; i < 12; i++ {
					colors[i] = [3]byte{45, 0, 0}
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
		lm.i2cFile.Close()
	}
}
