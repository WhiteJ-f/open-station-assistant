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

	log.Println("[ToF] Starting robust VL53L0X ranging monitor...")
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
	var palmCoverCount int
	var hoverCount int
	var lastVolChange time.Time
	var lastReportedVol int = -1

	// Range constants (mm)
	const (
		palmMuteMaxDist = 35  // <35mm palm cover
		hoverMinDist    = 40  // 40mm
		hoverMaxDist    = 100 // 100mm
		sliderMinDist   = 50  // 50mm
		sliderMaxDist   = 280 // 280mm
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

		// Filter invalid readings (8190 = out of range / timeout, 0 = hardware error)
		if dist <= 0 || dist >= 8000 {
			palmCoverCount = 0
			hoverCount = 0
			continue
		}

		// 1. Palm cover detection (<35mm for 3 consecutive samples = ~150ms)
		if dist < palmMuteMaxDist {
			palmCoverCount++
			if palmCoverCount == 3 {
				if cb.OnPalmCover != nil {
					cb.OnPalmCover()
				}
			}
			continue
		} else {
			palmCoverCount = 0
		}

		// 2. Continuous height volume slider (50mm - 280mm)
		if dist >= sliderMinDist && dist <= sliderMaxDist {
			if time.Since(lastVolChange) > 150*time.Millisecond {
				// Map 50mm..280mm -> 5%..100% volume
				pct := int(float64(dist-sliderMinDist) / float64(sliderMaxDist-sliderMinDist) * 95.0) + 5
				if pct < 5 {
					pct = 5
				}
				if pct > 100 {
					pct = 100
				}

				// Only report if changed by >= 5% to avoid jitter
				if lastReportedVol == -1 || absDiff(pct, lastReportedVol) >= 5 {
					lastReportedVol = pct
					lastVolChange = time.Now()
					if cb.OnVolumeChange != nil {
						cb.OnVolumeChange(pct)
					}
				}
			}
		}

		// 3. Hover wake detection (40-100mm held steady)
		if dist >= hoverMinDist && dist <= hoverMaxDist {
			hoverCount++
			if hoverCount == 12 { // ~600ms hover
				if cb.OnHoverWake != nil {
					cb.OnHoverWake()
				}
			}
		} else {
			hoverCount = 0
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
