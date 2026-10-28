package main

import (
	"bufio"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ToFCallbacks struct {
	OnPalmCover    func()
	OnVolumeChange func(int)
	OnHoverWake    func()
}

type ToFManager struct {
	cmd      *exec.Cmd
	stopChan chan struct{}
	mu       sync.Mutex
	enabled  bool
}

func NewToFManager(callbacks ToFCallbacks, enabled bool) *ToFManager {
	tm := &ToFManager{
		stopChan: make(chan struct{}),
		enabled:  enabled,
	}

	if !enabled {
		log.Println("[ToF] Laser proximity sensor DISABLED by configuration (testing mode)")
		return tm
	}

	log.Println("[ToF] Starting intelligent VL53L0X ranging monitor (delay + stationary rejection)...")
	go tm.monitorWorker(callbacks)
	return tm
}

func (tm *ToFManager) monitorWorker(cb ToFCallbacks) {
	cmd := exec.Command("/usr/bin/vl53l0x_ctrl", "-m", "crange", "-d", "999999999")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("[ToF] Failed to open pipe: %v", err)
		return
	}

	if err := cmd.Start(); err != nil {
		log.Printf("[ToF] Failed to start vl53l0x_ctrl: %v", err)
		return
	}

	tm.mu.Lock()
	tm.cmd = cmd
	tm.mu.Unlock()

	scanner := bufio.NewScanner(stdout)

	// Range constants (mm)
	const (
		palmMuteMaxDist = 35  // < 35mm palm cover
		sliderMinDist   = 50  // 50mm -> 5%
		sliderMaxDist   = 280 // 280mm -> 100%
		maxValidDist    = 380 // > 380mm considered empty room / out of reach
		activationDwell = 4   // ~280ms dwell before entering volume mode
	)

	var palmCoverCount int
	var dwellCount int
	var volumeModeActive bool
	var lastReportedVol int = -1
	var lastVolChange time.Time

	// Stationary object rejection tracking
	var history []int
	const historySize = 16 // ~1.1 seconds of samples
	var ignoredBaseline int = -1
	var stationarySince time.Time

	for scanner.Scan() {
		select {
		case <-tm.stopChan:
			_ = cmd.Process.Kill()
			return
		default:
		}

		line := strings.TrimSpace(scanner.Text())
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		dist, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}

		// 8190 = out of range / sensor open air
		if dist <= 0 || dist >= 8000 || dist > maxValidDist {
			palmCoverCount = 0
			dwellCount = 0
			if volumeModeActive {
				volumeModeActive = false
				log.Println("[ToF] Hand departed, volume adjustment mode locked.")
			}
			history = nil
			stationarySince = time.Time{}
			continue
		}

		// Check against ignored stationary baseline (e.g. shelf or ceiling)
		if ignoredBaseline > 0 {
			if absDiff(dist, ignoredBaseline) < 30 {
				// Still near the stationary object, keep ignoring
				continue
			} else {
				// Object moved significantly or was removed! Re-arm
				log.Printf("[ToF] Obstacle moved (dist: %d vs baseline: %d). Sensor re-armed.", dist, ignoredBaseline)
				ignoredBaseline = -1
				history = nil
			}
		}

		// 1. Palm cover detection (<35mm for 2 consecutive frames = ~140ms)
		if dist < palmMuteMaxDist {
			palmCoverCount++
			if palmCoverCount == 2 {
				log.Println("[ToF] GESTURE: Palm cover detected -> Mute / Stop playback!")
				if cb.OnPalmCover != nil {
					cb.OnPalmCover()
				}
			}
			dwellCount = 0
			volumeModeActive = false
			continue
		} else {
			palmCoverCount = 0
		}

		// Track stationary variance
		history = append(history, dist)
		if len(history) > historySize {
			history = history[1:]
		}

		if len(history) == historySize {
			minD, maxD := history[0], history[0]
			sumD := 0
			for _, d := range history {
				if d < minD {
					minD = d
				}
				if d > maxD {
					maxD = d
				}
				sumD += d
			}
			variance := maxD - minD
			if variance <= 14 {
				if stationarySince.IsZero() {
					stationarySince = time.Now()
				} else if time.Since(stationarySince) > 1200*time.Millisecond {
					// Stationary object detected for > 1.2s! Ignore it.
					ignoredBaseline = sumD / len(history)
					volumeModeActive = false
					dwellCount = 0
					log.Printf("[ToF] Stationary obstacle detected at %d mm -> Ignored as background.", ignoredBaseline)
					continue
				}
			} else {
				stationarySince = time.Time{}
			}
		}

		// 2. Volume control with activation dwell delay
		if dist >= sliderMinDist && dist <= sliderMaxDist {
			if !volumeModeActive {
				dwellCount++
				if dwellCount >= activationDwell {
					volumeModeActive = true
					log.Printf("[ToF] Dwell confirmed (%d mm) -> Volume control ACTIVE", dist)
				}
			}

			if volumeModeActive && time.Since(lastVolChange) > 80*time.Millisecond {
				// Map 50mm..280mm -> 5%..100%
				pct := int(float64(dist-sliderMinDist)/float64(sliderMaxDist-sliderMinDist)*95.0) + 5
				if pct < 5 {
					pct = 5
				}
				if pct > 100 {
					pct = 100
				}

				// Update if changed by >= 3% to balance smoothness and stability
				if lastReportedVol == -1 || absDiff(pct, lastReportedVol) >= 3 {
					lastReportedVol = pct
					lastVolChange = time.Now()
					if cb.OnVolumeChange != nil {
						cb.OnVolumeChange(pct)
					}
				}
			}
		} else {
			if !volumeModeActive {
				dwellCount = 0
			}
		}
	}

	_ = cmd.Wait()
}

func absDiff(a, b int) int {
	if a > b {
		return a - b
	}
	return b - a
}

func (tm *ToFManager) Close() {
	close(tm.stopChan)
	tm.mu.Lock()
	if tm.cmd != nil && tm.cmd.Process != nil {
		_ = tm.cmd.Process.Kill()
	}
	tm.mu.Unlock()
}
