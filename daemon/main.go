package main

import (
	"context"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

func main() {
	logFile, err := os.OpenFile("/tmp/assistant.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err == nil {
		log.SetOutput(io.MultiWriter(os.Stdout, logFile))
		defer logFile.Close()
	}

	log.Println("==================================================")
	log.Println("  Yandex Station Mini Standalone Assistant Daemon ")
	log.Println("==================================================")

	configPath := "/bkupgrade/data/config.json"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}
	cfg := LoadConfig(configPath)
	log.Printf("[Config] BaseURL: %s, STT: %s, LLM: %s, Voice: %s, Vol: %d, WakeWord: %s (thresh: %.2f)",
		cfg.GroqBaseURL, cfg.STTModel, cfg.LLMModel, cfg.TTSVoice, cfg.DefaultVolume, cfg.LocalWakeWordModel, cfg.WakeWordThreshold)

	leds := NewLEDManager()
	defer leds.Close()

	player := NewAudioPlayer(cfg)
	tts := NewTTSClient(cfg)
	ai := NewAICenter(cfg)
	audio := NewAudioCapture()
	defer audio.Close()

	spotter, err := NewLocalSpotter(cfg)
	if err != nil {
		log.Fatalf("[Daemon] Failed to initialize local openWakeWord spotter: %v", err)
	}
	defer spotter.Close()

	music := NewMusicService(player, leds)
	weather := NewWeatherService(cfg.City, cfg.Latitude, cfg.Longitude)
	intents := NewIntentEngine(cfg, player, leds, weather)

	// Cancellation coordination for palm cover & stop gestures
	var mu sync.Mutex
	var activeCancel context.CancelFunc
	var cancelRecord chan struct{}

	hoverWakeChan := make(chan struct{}, 1)

	tofCallbacks := ToFCallbacks{
		OnPalmCover: func() {
			log.Println("[Gesture] Hand over sensor detected (<45mm) -> STOP EVERYTHING TO STANDBY!")
			mu.Lock()
			if activeCancel != nil {
				activeCancel()
				activeCancel = nil
			}
			if cancelRecord != nil {
				select {
				case cancelRecord <- struct{}{}:
				default:
				}
			}
			mu.Unlock()

			player.Stop()
			player.Unmute()
			player.PlayCancel()
			leds.SetMode(ModeIdle)
			audio.SetPaused(false)
			spotter.Reset()
		},
		GetVolume: func() int {
			return player.GetVolume()
		},
		OnVolumeModeStart: func(vol int) {
			leds.ShowVolume(vol)
			player.PlayVolumeClick()
		},
		OnVolumeChange: func(vol int) {
			player.SetVolume(vol)
			leds.ShowVolume(vol)
			player.PlayVolumeClick()
		},
		OnVolumeHold: func(vol int) {
			leds.ShowVolume(vol)
		},
		OnHoverWake: func() {
			select {
			case hoverWakeChan <- struct{}{}:
			default:
			}
		},
	}

	if !cfg.SensorEnabled {
		_ = exec.Command("killall", "vl53l0x_ctrl").Run()
	}
	tof := NewToFManager(tofCallbacks, cfg.SensorEnabled)
	defer tof.Close()

	// Background feeder: stream 16kHz PCM from hardware mic pair to local openWakeWord spotter
	go func() {
		for chunk := range audio.Chan16k() {
			spotter.Feed16k(chunk)
		}
	}()

	// Visual and acoustic confirmation on startup:
	// 1. Purple ring pulse for 1s
	// 2. Unmute hardware amp & play authentic startup chime
	go leds.StartupFlash()
	audio.SetPaused(true)
	player.Unmute()
	player.PlayStartup()
	time.Sleep(300 * time.Millisecond)
	audio.SetPaused(false)
	log.Println("[Daemon] System ready. Listening locally for wake words (zero cloud traffic)...")

	// Main autonomous loop
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[Daemon] Recovered from panic: %v", r)
				}
			}()

			leds.SetMode(ModeIdle)

			// Wait for local wake word or hover gesture
			var wakeSource string
			select {
			case ev := <-spotter.WakeChan():
				wakeSource = ev.Word
				log.Printf("[Daemon] LOCAL WAKE WORD DETECTED: %s (confidence: %.2f)", ev.Word, ev.Confidence)
			case <-hoverWakeChan:
				wakeSource = "gesture"
				log.Println("[Daemon] GESTURE WAKE TRIGGERED")
			}

			_ = wakeSource

			ctx, cancel := context.WithCancel(context.Background())
			mu.Lock()
			activeCancel = cancel
			cancelRecord = make(chan struct{}, 1)
			mu.Unlock()

			defer func() {
				mu.Lock()
				if activeCancel != nil {
					activeCancel()
					activeCancel = nil
				}
				mu.Unlock()
			}()

			// 1. Wake word detected: Chime activates anytime it starts to listen!
			audio.SetPaused(true)
			spotter.Reset()
			player.PlayChime()
			time.Sleep(60 * time.Millisecond)
			leds.SetMode(ModeListening)
			audio.SetPaused(false)

			questionWav, err := audio.RecordSpeechWithVAD(7*time.Second, cancelRecord)
			if err != nil || ctx.Err() != nil {
				if ctx.Err() == nil && err == ErrNoSpeech {
					// User said nothing: play stop chime when it stops listening!
					player.PlayCancel()
				}
				leds.SetMode(ModeIdle)
				audio.SetPaused(false)
				return
			}

			// 2. Speech captured! Ring acknowledgment chime right as it starts streaming to cloud:
			player.PlayChime()
			audio.SetPaused(true)
			spotter.Reset()
			leds.SetMode(ModeThinking)

			// 3. Send recorded question to Groq Whisper STT
			query, err := ai.Transcribe(questionWav)
			if err != nil || strings.TrimSpace(query) == "" || ctx.Err() != nil {
				if ctx.Err() == nil {
					// Recognition failure / empty audio: play stop chime
					player.PlayCancel()
				}
				leds.SetMode(ModeIdle)
				audio.SetPaused(false)
				return
			}

			log.Printf("[User Query] \"%s\"", query)

			// Check for local stop words IMMEDIATELY before sending to LLM!
			if IsStopCommand(query) {
				log.Printf("[Daemon] Local stop command detected (\"%s\") -> Dismissing locally without cloud LLM query.", query)
				player.Stop()
				player.PlayCancel()
				leds.SetMode(ModeIdle)
				audio.SetPaused(false)
				return
			}

			// 4. Check deterministic local intent first (Time, Date, Volume, Playback, Weather)
			intentRes, _ := intents.TryHandle(query)
			if intentRes.Handled {
				log.Printf("[Intent] Handled locally: %s -> Answer: \"%s\"", intentRes.Action, intentRes.Answer)
				if intentRes.Action == "STOP" || intentRes.Action == "VOLUME" {
					leds.SetMode(ModeIdle)
					audio.SetPaused(false)
					return
				}

				if intentRes.Answer != "" && ctx.Err() == nil {
					speakAnswer(ctx, intentRes.Answer, leds, player, audio, tts)
				}
			} else {
				// 5. Query Groq LLM (Qwen / Llama) for conversational answers
				leds.SetMode(ModeThinking)
				llmResp, err := ai.AskLLM(query)
				if err != nil || ctx.Err() != nil {
					if ctx.Err() == nil {
						speakAnswer(ctx, "Произошла ошибка при обращении к серверу.", leds, player, audio, tts)
						player.PlayCancel()
					}
					leds.SetMode(ModeIdle)
					audio.SetPaused(false)
					return
				}

				log.Printf("[LLM] Intent: %s, Answer: \"%s\"", llmResp.Intent, llmResp.Answer)

				switch llmResp.Intent {
				case "MUSIC":
					_ = music.PlayQuery(llmResp.Param)
				case "STOP":
					player.Stop()
					player.PlayCancel()
					leds.SetMode(ModeIdle)
					audio.SetPaused(false)
					return
				case "VOLUME":
					if newVol, err := strconv.Atoi(llmResp.Param); err == nil {
						player.SetVolume(newVol)
						leds.ShowVolume(newVol)
						player.PlayVolumeClick()
					}
				default:
					if ctx.Err() == nil {
						speakAnswer(ctx, llmResp.Answer, leds, player, audio, tts)
					}
				}
			}

			if ctx.Err() != nil {
				leds.SetMode(ModeIdle)
				audio.SetPaused(false)
				return
			}

			// 6. Continuous Conversation: Listen for follow-up questions for 5.5 seconds!
			for followUp := 0; followUp < 2; followUp++ {
				if ctx.Err() != nil {
					break
				}

				leds.SetMode(ModeListening)
				audio.SetPaused(false)

				followWav, err := audio.RecordSpeechWithVAD(5500*time.Millisecond, cancelRecord)
				if err != nil || len(followWav) == 0 || ctx.Err() != nil {
					if ctx.Err() == nil {
						player.PlayCancel()
					}
					break
				}

				// Speech captured in follow-up! Play chime right as it starts streaming to cloud:
				player.PlayChime()
				audio.SetPaused(true)
				spotter.Reset()
				leds.SetMode(ModeThinking)

				followQuery, err := ai.Transcribe(followWav)
				if err != nil || strings.TrimSpace(followQuery) == "" || ctx.Err() != nil {
					if ctx.Err() == nil {
						player.PlayCancel()
					}
					break
				}
				log.Printf("[Follow-Up] \"%s\"", followQuery)

				// LOCAL STOP WORDS CHECK IN CONVERSATION:
				// Do not send anywhere. Just stop.
				if IsStopCommand(followQuery) {
					log.Printf("[Follow-Up] Local stop word detected (\"%s\") -> Ending conversation locally without cloud LLM query.", followQuery)
					player.Stop()
					player.PlayCancel()
					break
				}

				if fIntent, _ := intents.TryHandle(followQuery); fIntent.Handled {
					if fIntent.Action == "STOP" {
						player.PlayCancel()
						break
					}
					if fIntent.Answer != "" && ctx.Err() == nil {
						speakAnswer(ctx, fIntent.Answer, leds, player, audio, tts)
					}
					continue
				}

				fResp, err := ai.AskLLM(followQuery)
				if err != nil || ctx.Err() != nil {
					if ctx.Err() == nil {
						player.PlayCancel()
					}
					break
				}
				if fResp.Intent == "STOP" {
					player.PlayCancel()
					break
				}
				if ctx.Err() == nil {
					speakAnswer(ctx, fResp.Answer, leds, player, audio, tts)
				}
			}

			leds.SetMode(ModeIdle)
			audio.SetPaused(false)
		}()
	}
}

func speakAnswer(ctx context.Context, answer string, leds *LEDManager, player *AudioPlayer, audio *AudioCapture, tts *TTSClient) {
	if ctx.Err() != nil {
		return
	}
	leds.SetMode(ModeSpeaking)
	outputWAV := "/tmp/tts_response.wav"
	currentVol := player.GetVolume()
	start := time.Now()
	if err := tts.SynthesizeToWAV(answer, outputWAV, currentVol); err != nil {
		log.Printf("[TTS] Synthesis error: %v", err)
		return
	}
	if ctx.Err() != nil {
		return
	}
	log.Printf("[TTS] Synthesized in %v: \"%s\"", time.Since(start), answer)
	audio.SetPaused(true)
	_ = player.PlayWithVU(outputWAV, leds.SetVoiceBrightness)
	time.Sleep(50 * time.Millisecond)
}
