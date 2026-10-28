package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

type WakeEvent struct {
	Word       string
	Confidence float64
}

type LocalSpotter struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	wakeChan chan WakeEvent
	stopChan chan struct{}
	mu       sync.Mutex
	audioBuf []byte
}

func NewLocalSpotter(cfg *Config) (*LocalSpotter, error) {
	binPath := "/bkupgrade/bin/station-spotter"
	if _, err := os.Stat(binPath); err != nil {
		return nil, fmt.Errorf("station-spotter binary not found at %s: %w", binPath, err)
	}

	words := cfg.LocalWakeWordModel
	if words == "" {
		words = "alexa,jarvis"
	}
	thresh := cfg.WakeWordThreshold
	if thresh <= 0.0 || thresh >= 1.0 {
		thresh = 0.35
	}

	cmd := exec.Command(binPath,
		"-w", words,
		"-t", fmt.Sprintf("%.2f", thresh),
		"-r", "2000",
	)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe error: %w", err)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdinPipe.Close()
		return nil, fmt.Errorf("stdout pipe error: %w", err)
	}

	// Route stderr to daemon log so we see model loading and startup info
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		_ = stdinPipe.Close()
		return nil, fmt.Errorf("failed to start station-spotter: %w", err)
	}

	ls := &LocalSpotter{
		cmd:      cmd,
		stdin:    stdinPipe,
		wakeChan: make(chan WakeEvent, 5),
		stopChan: make(chan struct{}),
		audioBuf: make([]byte, 0, 2560),
	}

	go ls.readLoop(stdoutPipe)

	log.Printf("[LocalSpotter] Spawned %s (PID %d, models: %s, threshold: %.2f)",
		binPath, cmd.Process.Pid, words, thresh)
	return ls, nil
}

func (ls *LocalSpotter) WakeChan() <-chan WakeEvent {
	return ls.wakeChan
}

func (ls *LocalSpotter) Feed16k(chunk []int16) {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	if ls.stdin == nil {
		return
	}

	// Pack int16 samples into byte buffer
	for _, s := range chunk {
		ls.audioBuf = binary.LittleEndian.AppendUint16(ls.audioBuf, uint16(s))
	}

	// station-spotter processes 1280 samples = 2560 bytes (80ms chunks)
	for len(ls.audioBuf) >= 2560 {
		frame := ls.audioBuf[:2560]
		_, err := ls.stdin.Write(frame)
		ls.audioBuf = ls.audioBuf[2560:]
		if err != nil {
			log.Printf("[LocalSpotter] Pipe write error: %v", err)
			return
		}
	}
}

func (ls *LocalSpotter) Reset() {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.audioBuf = ls.audioBuf[:0]
	// Drain any pending events
	for len(ls.wakeChan) > 0 {
		<-ls.wakeChan
	}
}

func (ls *LocalSpotter) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		// Example: "WAKE jarvis 0.982" or "WAKE alexa 0.891"
		if strings.HasPrefix(line, "WAKE ") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				word := parts[1]
				conf := 1.0
				if len(parts) >= 3 {
					if c, err := strconv.ParseFloat(parts[2], 64); err == nil {
						conf = c
					}
				}
				log.Printf("[LocalSpotter] >>> Wake detection: %s (confidence: %.3f) <<<", word, conf)
				select {
				case ls.wakeChan <- WakeEvent{Word: word, Confidence: conf}:
				default:
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		select {
		case <-ls.stopChan:
		default:
			log.Printf("[LocalSpotter] Stdout scanner closed: %v", err)
		}
	}
}

func (ls *LocalSpotter) Close() {
	close(ls.stopChan)
	ls.mu.Lock()
	if ls.stdin != nil {
		_ = ls.stdin.Close()
		ls.stdin = nil
	}
	if ls.cmd != nil && ls.cmd.Process != nil {
		_ = ls.cmd.Process.Kill()
	}
	ls.mu.Unlock()
}
