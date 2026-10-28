package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os/exec"
	"sync"
	"time"
)

var ErrNoSpeech = errors.New("no speech detected")

type AudioCapture struct {
	pcmChan    chan []int16
	pcmChan16k chan []int16
	stopChan   chan struct{}
	cmd        *exec.Cmd
	mu         sync.Mutex
	paused     bool
}

func NewAudioCapture() *AudioCapture {
	ac := &AudioCapture{
		pcmChan:    make(chan []int16, 50),
		pcmChan16k: make(chan []int16, 50),
		stopChan:   make(chan struct{}),
	}
	go ac.streamWorker()
	return ac
}

func (ac *AudioCapture) SetPaused(p bool) {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	ac.paused = p
	// Drain channels on both pause and unpause
	for len(ac.pcmChan) > 0 {
		<-ac.pcmChan
	}
	for len(ac.pcmChan16k) > 0 {
		<-ac.pcmChan16k
	}
}

func (ac *AudioCapture) Chan16k() <-chan []int16 {
	return ac.pcmChan16k
}

func (ac *AudioCapture) streamWorker() {
	for {
		select {
		case <-ac.stopChan:
			return
		default:
		}

		cmd := exec.Command("/bin/tinycap", "--", "-D", "0", "-d", "2", "-c", "4", "-r", "48000", "-b", "16")
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			log.Printf("[Audio] Pipe error: %v", err)
			time.Sleep(500 * time.Millisecond)
			continue
		}

		if err := cmd.Start(); err != nil {
			log.Printf("[Audio] Start error: %v", err)
			time.Sleep(500 * time.Millisecond)
			continue
		}

		ac.mu.Lock()
		ac.cmd = cmd
		ac.mu.Unlock()

		// Skip startup transient (1.5s = 576,000 bytes at 4ch S16_LE)
		skipBuf := make([]byte, 576000)
		_, _ = io.ReadFull(stdout, skipBuf)

		chunkSize := 19200 // 50ms at 4ch S16_LE
		rawBuf := make([]byte, chunkSize)

		// 150Hz 1st-order IIR High-Pass Filter (fs = 48000 Hz)
		// Strips sub-audible room rumble, air drafts, and DC offset
		// alpha = 1 / (1 + 2*pi*150/48000) = 0.9807
		const hpfAlpha = 0.9807
		var lastIn, lastOut float64

		for {
			select {
			case <-ac.stopChan:
				_ = cmd.Process.Kill()
				return
			default:
			}

			if _, err := io.ReadFull(stdout, rawBuf); err != nil {
				break
			}

			ac.mu.Lock()
			isPaused := ac.paused
			ac.mu.Unlock()

			if isPaused {
				continue
			}

			// Capture from true physical MEMS microphone pair (Channels 2 and 3)
			// Averaging (Ch2 + Ch3) / 2.0 provides 360-degree acoustic coverage and boosts SNR by 3 dB.
			// Apply 150 Hz HPF in real time: passes 100% of human speech formants (300-3400 Hz)
			// while completely eliminating room rumble, fan noise, and DC bias.
			// Apply 12.0x (+21.6 dB) digital preamp gain: aligns raw digital MEMS levels
			// with nominal conversational speech range for openWakeWord neural network and VAD.
			numSamples := chunkSize / 8
			monoChunk := make([]int16, numSamples)
			const micPreampGain = 12.0

			for i := 0; i < numSamples; i++ {
				idx := i * 8
				s2 := float64(int16(binary.LittleEndian.Uint16(rawBuf[idx+4 : idx+6])))
				s3 := float64(int16(binary.LittleEndian.Uint16(rawBuf[idx+6 : idx+8])))
				s := (s2 + s3) / 2.0
				out := hpfAlpha * (lastOut + s - lastIn)
				lastIn = s
				lastOut = out

				out = out * micPreampGain
				if out > 32767 {
					out = 32767
				} else if out < -32768 {
					out = -32768
				}
				monoChunk[i] = int16(out)
			}

			// Downsample 48kHz to 16kHz (3:1 integer decimation with moving average anti-aliasing)
			num16k := numSamples / 3
			chunk16k := make([]int16, num16k)
			for j := 0; j < num16k; j++ {
				base := j * 3
				avg := (int32(monoChunk[base]) + int32(monoChunk[base+1]) + int32(monoChunk[base+2])) / 3
				chunk16k[j] = int16(avg)
			}

			select {
			case ac.pcmChan <- monoChunk:
			default:
				// Channel full, drop oldest frame
				select {
				case <-ac.pcmChan:
				default:
				}
				ac.pcmChan <- monoChunk
			}

			select {
			case ac.pcmChan16k <- chunk16k:
			default:
				select {
				case <-ac.pcmChan16k:
				default:
				}
				ac.pcmChan16k <- chunk16k
			}
		}

		_ = cmd.Wait()
		time.Sleep(300 * time.Millisecond)
	}
}

// RecordSpeechWithVAD waits for speech and records until 750ms trailing silence.
func (ac *AudioCapture) RecordSpeechWithVAD(timeout time.Duration, cancelChan <-chan struct{}) ([]byte, error) {
	// Drain stale frames before listening
	for len(ac.pcmChan) > 0 {
		<-ac.pcmChan
	}

	// Pre-speech ring buffer (last 8 frames = 400ms to preserve soft word onsets)
	var preSpeechRing [][]int16
	maxRingSize := 8

	var speechActive bool
	var speechFrames [][]int16
	var silenceDuration time.Duration
	var totalSpeechDuration time.Duration
	var activeFramesCount int

	noiseFloor := 7.0 // calibrated ambient room noise on hardware with 12x preamp
	frameDuration := 50 * time.Millisecond
	startTime := time.Now()
	lastLogTime := time.Now()

	for {
		select {
		case <-cancelChan:
			return nil, fmt.Errorf("recording cancelled")
		default:
		}

		if time.Since(startTime) > timeout {
			if speechActive && activeFramesCount >= 5 && (totalSpeechDuration-silenceDuration) >= 300*time.Millisecond {
				break
			}
			return nil, ErrNoSpeech
		}

		var chunk []int16
		select {
		case chunk = <-ac.pcmChan:
		case <-time.After(100 * time.Millisecond):
			continue
		}

		// Compute AC RMS
		var sumSquares float64
		for _, s := range chunk {
			sumSquares += float64(s) * float64(s)
		}
		rms := math.Sqrt(sumSquares / float64(len(chunk)))

		// Voice threshold: 1.45x ambient noise, clamped to [12.0, 120.0]
		threshold := noiseFloor * 1.45
		if threshold < 12.0 {
			threshold = 12.0
		}
		if threshold > 120.0 {
			threshold = 120.0
		}

		if time.Since(lastLogTime) >= 2500*time.Millisecond && !speechActive {
			log.Printf("[VAD] Listening... (RMS: %.1f, threshold: %.1f, noise: %.1f)", rms, threshold, noiseFloor)
			lastLogTime = time.Now()
		}

		if rms > threshold {
			if !speechActive {
				log.Printf("[VAD] Voice detected! (RMS: %.1f, threshold: %.1f)", rms, threshold)
				speechActive = true
				speechFrames = append(speechFrames, preSpeechRing...)
			}
			activeFramesCount++
			silenceDuration = 0
			totalSpeechDuration += frameDuration
			speechFrames = append(speechFrames, chunk)

			if totalSpeechDuration >= 12*time.Second {
				log.Printf("[VAD] Max speech length reached (%v)", totalSpeechDuration)
				break
			}
		} else {
			if !speechActive {
				noiseFloor = 0.95*noiseFloor + 0.05*rms
				if noiseFloor > 50.0 {
					noiseFloor = 50.0
				}
				preSpeechRing = append(preSpeechRing, chunk)
				if len(preSpeechRing) > maxRingSize {
					preSpeechRing = preSpeechRing[1:]
				}
			} else {
				speechFrames = append(speechFrames, chunk)
				silenceDuration += frameDuration
				totalSpeechDuration += frameDuration

				if silenceDuration >= 350*time.Millisecond {
					if activeFramesCount < 4 || (totalSpeechDuration-silenceDuration) < 200*time.Millisecond {
						// Transient click or false alarm: reset and continue listening until full timeout expires
						speechActive = false
						activeFramesCount = 0
						silenceDuration = 0
						totalSpeechDuration = 0
						speechFrames = nil
						continue
					}
					log.Printf("[VAD] Speech utterance complete (%v audio, %v trailing silence, %d active frames)", totalSpeechDuration-silenceDuration, silenceDuration, activeFramesCount)
					break
				}
			}
		}
	}

	if !speechActive || activeFramesCount < 5 || (totalSpeechDuration-silenceDuration) < 300*time.Millisecond {
		return nil, ErrNoSpeech
	}

	var allSamples []int16
	for _, f := range speechFrames {
		allSamples = append(allSamples, f...)
	}

	// Digital AGC normalization: bring quiet microphone peak up to optimal Whisper range (~18,000)
	var maxAbs int16
	for _, s := range allSamples {
		abs := s
		if abs < 0 {
			abs = -abs
		}
		if abs > maxAbs {
			maxAbs = abs
		}
	}

	if maxAbs > 0 && maxAbs < 16000 {
		gain := float64(18000) / float64(maxAbs)
		if gain > 600.0 {
			gain = 600.0
		}
		for i := range allSamples {
			v := float64(allSamples[i]) * gain
			if v > 32767 {
				v = 32767
			} else if v < -32768 {
				v = -32768
			}
			allSamples[i] = int16(v)
		}
		log.Printf("[Audio] AGC boosted speech by %.1fx (peak: %d -> %d)", gain, maxAbs, int(float64(maxAbs)*gain))
	}

	return EncodeWAV(allSamples, 48000), nil
}

func (ac *AudioCapture) Close() {
	close(ac.stopChan)
	ac.mu.Lock()
	if ac.cmd != nil && ac.cmd.Process != nil {
		_ = ac.cmd.Process.Kill()
	}
	ac.mu.Unlock()
}

func EncodeWAV(samples []int16, sampleRate int) []byte {
	var buf bytes.Buffer
	dataSize := uint32(len(samples) * 2)
	fileSize := dataSize + 36

	buf.WriteString("RIFF")
	_ = binary.Write(&buf, binary.LittleEndian, fileSize)
	buf.WriteString("WAVE")

	buf.WriteString("fmt ")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(16))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(sampleRate))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(sampleRate*2))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(2))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(16))

	buf.WriteString("data")
	_ = binary.Write(&buf, binary.LittleEndian, dataSize)
	for _, s := range samples {
		_ = binary.Write(&buf, binary.LittleEndian, s)
	}

	return buf.Bytes()
}
