package main

import (
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
	ModeMute
)

type LEDManager struct {
	mu          sync.Mutex
	currentMode LEDMode
	volumeLevel int
	volumeTimer *time.Timer
	stopChan    chan struct{}
	lastMode    LEDMode
	i2cFile     *os.File
}

func NewLEDManager() *LEDManager {
	lm := &LEDManager{
		currentMode: ModeIdle,
		lastMode:    ModeIdle,
		volumeLevel: 35,
		stopChan:    make(chan struct{}),
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

func (lm *LEDManager) animationLoop() {
	var step int
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-lm.stopChan:
			return
		case <-ticker.C:
			lm.mu.Lock()
			mode := lm.currentMode
			vol := lm.volumeLevel
			lm.mu.Unlock()

			var colors [12][3]byte
			step++

			switch mode {
			case ModeOff:
				lm.writeLEDs(colors)

			case ModeIdle:
				// Subtle breathing purple on 4 cardinal LEDs
				brightness := byte(4 + (step%8))
				colors[0] = [3]byte{brightness, 0, brightness * 2}
				colors[3] = [3]byte{brightness, 0, brightness * 2}
				colors[6] = [3]byte{brightness, 0, brightness * 2}
				colors[9] = [3]byte{brightness, 0, brightness * 2}
				lm.writeLEDs(colors)

			case ModeListening:
				// Solid / breathing bright purple ring
				pulse := byte(15 + (step%6)*2)
				for i := 0; i < 12; i++ {
					colors[i] = [3]byte{pulse, 0, pulse + 12}
				}
				lm.writeLEDs(colors)

			case ModeThinking:
				// Smooth clockwise spinning cyan/purple dot animation
				head := step % 12
				tail1 := (head + 11) % 12
				tail2 := (head + 10) % 12
				colors[head] = [3]byte{0, 25, 30}
				colors[tail1] = [3]byte{0, 10, 16}
				colors[tail2] = [3]byte{0, 3, 6}
				lm.writeLEDs(colors)

			case ModeSpeaking:
				// Gentle wave across ring
				for i := 0; i < 12; i++ {
					offset := (step + i) % 12
					b := byte(offset * 2)
					colors[i] = [3]byte{b, 0, b * 2}
				}
				lm.writeLEDs(colors)

			case ModeVolume:
				// Radial gauge: 1 to 12 LEDs bright purple
				numLEDs := vol * 12 / 100
				if numLEDs < 1 && vol > 0 {
					numLEDs = 1
				}
				if numLEDs > 12 {
					numLEDs = 12
				}
				for i := 0; i < 12; i++ {
					if i < numLEDs {
						colors[i] = [3]byte{20, 0, 30}
					}
				}
				lm.writeLEDs(colors)

			case ModeMute:
				// Red ring pulse
				for i := 0; i < 12; i++ {
					colors[i] = [3]byte{30, 0, 0}
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
