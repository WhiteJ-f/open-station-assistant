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
	OnPalmCover       func()
	GetVolume         func() int
	OnVolumeModeStart func(int)
	OnVolumeChange    func(int)
	OnHoverWake       func()
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
		log.Println("[ToF] Laser proximity sensor DISABLED by configuration")
		return tm
	}

	log.Println("[ToF] Starting authentic VL53L0X ranging monitor (activation dwell + 10-step discrete tracking + palm stop)...")
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

	// Thresholds and geometric bounds (in millimeters)
	const (
		palmMuteMaxDist   = 42  // < 42mm hand flat over sensor (stop to standby)
		volumeMinDist     = 65  // 65mm lower bound for hand height
		volumeMaxDist     = 340 // 340mm upper bound for hand height
		openAirMinDist    = 370 // > 370mm considered open air above speaker
		activationDwellMs = 300 // 300ms stable hand dwell BEFORE entering volume mode
		stepHeightMm      = 22  // ~22mm vertical hand displacement per volume level (1..10)
	)

	var (
		palmFrames int

		// Activation dwell state
		dwellAnchorDist int
		dwellStartTime  time.Time

		// Active volume state
		volumeModeActive  bool
		anchorDist        int
		anchorLevel       int // 1..10
		lastReportedLevel int

		// Hand departure detection
		openAirFrames int

		// Stationary obstacle rejection
		stationaryHistory []int
		stationaryStart   time.Time
		ignoredMin        int = -1
		ignoredMax        int = -1
	)

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

		// 8190 = sensor out of range / open air
		isOpenAir := (dist <= 0 || dist >= 8000 || dist > openAirMinDist)

		if isOpenAir {
			openAirFrames++
			dwellStartTime = time.Time{}
			palmFrames = 0

			// Require ~120ms (4 frames) of continuous open air to confirm hand departure
			if openAirFrames >= 4 {
				if volumeModeActive {
					volumeModeActive = false
					log.Printf("[ToF] Hand departed -> Volume adjustment mode locked at %d%%.", lastReportedLevel*10)
				}
				// Reset ignored stationary baseline once clear open air is confirmed
				if openAirFrames >= 15 {
					ignoredMin = -1
					ignoredMax = -1
					stationaryHistory = nil
					stationaryStart = time.Time{}
				}
			}
			continue
		}

		openAirFrames = 0

		// 1. Stationary obstacle filter (ignores ceilings, shelves, resting objects)
		if ignoredMin > 0 && dist >= ignoredMin && dist <= ignoredMax {
			dwellStartTime = time.Time{}
			palmFrames = 0
			continue
		}

		// 2. PALM COVER / HAND OVER SENSOR (< 42mm):
		// Immediate gesture: STOP ANY RUNNING FUNCTION & RETURN TO STANDBY LISTENING!
		if dist < palmMuteMaxDist {
			palmFrames++
			if palmFrames == 2 { // Confirmed over 2 consecutive frames (~60ms)
				log.Println("[ToF] GESTURE: Hand-over-sensor (<42mm) -> STOP ALL TO STANDBY!")
				if cb.OnPalmCover != nil {
					cb.OnPalmCover()
				}
			}
			dwellStartTime = time.Time{}
			volumeModeActive = false
			continue
		} else {
			palmFrames = 0
		}

		// 3. Stationary background obstacle detection:
		// If height is unchanged (variance <= 12mm) for >= 1000ms while not actively adjusting volume, ignore it.
		if !volumeModeActive {
			stationaryHistory = append(stationaryHistory, dist)
			if len(stationaryHistory) > 12 {
				stationaryHistory = stationaryHistory[1:]
			}
			if len(stationaryHistory) == 12 {
				minD, maxD := stationaryHistory[0], stationaryHistory[0]
				for _, d := range stationaryHistory {
					if d < minD {
						minD = d
					}
					if d > maxD {
						maxD = d
					}
				}
				if (maxD - minD) <= 12 {
					if stationaryStart.IsZero() {
						stationaryStart = time.Now()
					} else if time.Since(stationaryStart) >= 1000*time.Millisecond {
						ignoredMin = minD - 20
						ignoredMax = maxD + 20
						dwellStartTime = time.Time{}
						log.Printf("[ToF] Stationary surface detected [%d..%d mm] -> Ignored as background obstacle.", ignoredMin, ignoredMax)
						continue
					}
				} else {
					stationaryStart = time.Time{}
				}
			}
		}

		// 4. VOLUME CONTROL ENGINE:
		if dist >= volumeMinDist && dist <= volumeMaxDist {
			if !volumeModeActive {
				// PHASE A: DWELL DELAY BEFORE ACTIVATING
				// Hand must enter volume zone and remain relatively steady for activationDwellMs
				if dwellStartTime.IsZero() || absDiff(dist, dwellAnchorDist) > 25 {
					dwellAnchorDist = dist
					dwellStartTime = time.Now()
				} else if time.Since(dwellStartTime) >= activationDwellMs*time.Millisecond {
					// DWELL CONFIRMED! Activate volume mode immediately!
					volumeModeActive = true
					anchorDist = dist

					currentVol := 15
					if cb.GetVolume != nil {
						currentVol = cb.GetVolume()
					}
					// Map 0..100% to discrete levels 1..10
					anchorLevel = (currentVol + 5) / 10
					if anchorLevel < 1 {
						anchorLevel = 1
					}
					if anchorLevel > 10 {
						anchorLevel = 10
					}
					lastReportedLevel = anchorLevel

					log.Printf("[ToF] Dwell confirmed: anchor at %d mm (level %d/10, %d%%) -> Volume mode ACTIVE", dist, anchorLevel, anchorLevel*10)

					if cb.OnVolumeModeStart != nil {
						cb.OnVolumeModeStart(anchorLevel * 10)
					}
				}
			} else {
				// PHASE B: ACTIVE VOLUME MODE (ZERO DELAY)
				// Hand movement relative to initial anchor distance
				deltaDist := dist - anchorDist
				stepChange := deltaDist / stepHeightMm

				newLevel := anchorLevel + stepChange
				if newLevel < 1 {
					newLevel = 1
				}
				if newLevel > 10 {
					newLevel = 10
				}

				if newLevel != lastReportedLevel {
					lastReportedLevel = newLevel
					newVol := newLevel * 10
					if cb.OnVolumeChange != nil {
						cb.OnVolumeChange(newVol)
					}
				}
			}
		} else {
			if !volumeModeActive {
				dwellStartTime = time.Time{}
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
