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

	log.Println("[ToF] Starting authentic VL53L0X ranging monitor (5-frame filter + 280ms dwell + 10 discrete steps)...")
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
		palmMuteMaxDist   = 45  // < 45mm hand flat over sensor (stop to standby)
		volumeMinDist     = 60  // 60mm lower bound for hand height
		volumeMaxDist     = 350 // 350mm upper bound for hand height
		openAirMinDist    = 380 // > 380mm considered open air above speaker
		activationDwellMs = 280 // 280ms stable hand dwell BEFORE entering volume mode
		stepHeightMm      = 22  // ~22mm vertical hand displacement per volume level (1..10)
	)

	var (
		palmFrames int

		// 4-frame moving average to smooth sensor optical jitter
		smoothWindow []int

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

		rawDist, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}

		// 8190 = sensor out of range / open air
		isOpenAir := (rawDist <= 0 || rawDist >= 8000 || rawDist > openAirMinDist)

		if isOpenAir {
			openAirFrames++
			smoothWindow = nil
			dwellStartTime = time.Time{}
			palmFrames = 0

			// Require ~120ms (4 frames) of continuous open air to confirm hand departure
			if openAirFrames >= 4 {
				if volumeModeActive {
					volumeModeActive = false
					log.Printf("[ToF] Hand departed -> Volume locked at %d%%.", lastReportedLevel*10)
				}
				// Reset ignored stationary baseline once clear open air is confirmed for >500ms
				if openAirFrames >= 16 {
					ignoredMin = -1
					ignoredMax = -1
					stationaryHistory = nil
					stationaryStart = time.Time{}
				}
			}
			continue
		}

		openAirFrames = 0

		// PALM COVER / HAND OVER SENSOR (< 45mm):
		// Immediate gesture: STOP ANY RUNNING FUNCTION & RETURN TO STANDBY LISTENING!
		if rawDist < palmMuteMaxDist {
			palmFrames++
			if palmFrames == 2 {
				log.Println("[ToF] GESTURE: Hand-over-sensor (<45mm) -> STOP ALL TO STANDBY!")
				if cb.OnPalmCover != nil {
					cb.OnPalmCover()
				}
			}
			dwellStartTime = time.Time{}
			volumeModeActive = false
			smoothWindow = nil
			continue
		} else {
			palmFrames = 0
		}

		// Moving average filter to eliminate optical noise (window size 4)
		smoothWindow = append(smoothWindow, rawDist)
		if len(smoothWindow) > 4 {
			smoothWindow = smoothWindow[1:]
		}
		var sumDist int
		for _, d := range smoothWindow {
			sumDist += d
		}
		dist := sumDist / len(smoothWindow)

		// 1. Stationary obstacle filter (ignores ceilings, shelves, resting objects)
		if ignoredMin > 0 && dist >= ignoredMin && dist <= ignoredMax {
			dwellStartTime = time.Time{}
			continue
		}

		// 2. Stationary background obstacle detection:
		// If height is unchanged (variance <= 12mm) for >= 1000ms while not actively adjusting volume, ignore it.
		if !volumeModeActive {
			stationaryHistory = append(stationaryHistory, dist)
			if len(stationaryHistory) > 14 {
				stationaryHistory = stationaryHistory[1:]
			}
			if len(stationaryHistory) == 14 {
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

		// 3. VOLUME CONTROL ENGINE:
		if dist >= volumeMinDist && dist <= volumeMaxDist {
			if !volumeModeActive {
				// PHASE A: DWELL DELAY BEFORE ACTIVATING
				// Hand must enter volume zone and remain relatively steady (+/- 35mm) for 280ms
				if dwellStartTime.IsZero() || absDiff(dist, dwellAnchorDist) > 35 {
					dwellAnchorDist = dist
					dwellStartTime = time.Now()
				} else if time.Since(dwellStartTime) >= activationDwellMs*time.Millisecond {
					// DWELL CONFIRMED! Activate volume mode immediately!
					volumeModeActive = true
					anchorDist = dist

					currentVol := 30
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
