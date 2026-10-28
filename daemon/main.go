package main

import (
	"io"
	"log"
	"os"
	"strconv"
	"strings"
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

	var cancelRecord chan struct{}
	hoverWakeChan := make(chan struct{}, 1)

	tofCallbacks := ToFCallbacks{
		OnPalmCover: func() {
			log.Println("[Gesture] Palm cover detected (<35mm) -> MUTE / STOP")
			player.Stop()
			player.Mute()
			leds.SetMode(ModeMute)
			if cancelRecord != nil {
				select {
				case cancelRecord <- struct{}{}:
				default:
				}
			}
			time.Sleep(500 * time.Millisecond)
			leds.SetMode(ModeIdle)
		},
		OnVolumeChange: func(vol int) {
			player.SetVolume(vol)
			leds.ShowVolume(vol)
		},
		OnHoverWake: func() {
			select {
			case hoverWakeChan <- struct{}{}:
			default:
			}
		},
	}

	tof := NewToFManager(tofCallbacks, cfg.SensorEnabled)
	defer tof.Close()

	// Background feeder: stream 16kHz PCM from hardware mic pair to local openWakeWord spotter
	go func() {
		for chunk := range audio.Chan16k() {
			spotter.Feed16k(chunk)
		}
	}()

	// Play authentic startup chime
	audio.SetPaused(true)
	player.PlayChime()
	time.Sleep(400 * time.Millisecond)
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

			// 1. INSTANT CHIME (<50ms): Authentic "Ту-дум!" rings immediately!
			audio.SetPaused(true)
			spotter.Reset()
			player.PlayChime()
			time.Sleep(50 * time.Millisecond)

			// 2. Set LED to listening mode and record user's question with VAD
			leds.SetMode(ModeListening)
			audio.SetPaused(false)

			cancelRecord = make(chan struct{}, 1)
			questionWav, err := audio.RecordSpeechWithVAD(7*time.Second, cancelRecord)
			if err != nil {
				if err != ErrNoSpeech {
					log.Printf("[VAD] %v", err)
				}
				leds.SetMode(ModeIdle)
				return
			}

			// 3. Pause audio during network processing and response playback
			audio.SetPaused(true)
			spotter.Reset()
			leds.SetMode(ModeThinking)

			// 4. Send ONLY the recorded question to Groq Whisper STT
			query, err := ai.Transcribe(questionWav)
			if err != nil || strings.TrimSpace(query) == "" {
				log.Printf("[STT] Empty or error: %v", err)
				leds.SetMode(ModeIdle)
				audio.SetPaused(false)
				return
			}

			log.Printf("[User Query] \"%s\"", query)

			// 5. Try deterministic local intent first (Time, Date, Volume, Playback, Weather)
			// Zero LLM latency, 100% privacy!
			intentRes, _ := intents.TryHandle(query)
			if intentRes.Handled {
				log.Printf("[Intent] Handled locally: %s -> Answer: \"%s\"", intentRes.Action, intentRes.Answer)
				if intentRes.Action == "STOP" || intentRes.Action == "VOLUME" {
					leds.SetMode(ModeIdle)
					audio.SetPaused(false)
					return
				}

				if intentRes.Answer != "" {
					speakAnswer(intentRes.Answer, leds, player, audio, tts)
				}
				leds.SetMode(ModeIdle)
				audio.SetPaused(false)
				return
			}

			// 6. Query Groq LLM (Qwen / Llama) for conversational answers
			leds.SetMode(ModeThinking)
			llmResp, err := ai.AskLLM(query)
			if err != nil {
				log.Printf("[LLM] Error: %v", err)
				speakAnswer("Произошла ошибка при обращении к серверу.", leds, player, audio, tts)
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
				leds.SetMode(ModeIdle)
			case "VOLUME":
				if newVol, err := strconv.Atoi(llmResp.Param); err == nil {
					player.SetVolume(newVol)
					leds.ShowVolume(newVol)
					player.PlayVolumeClick()
				}
			default:
				speakAnswer(llmResp.Answer, leds, player, audio, tts)
			}

			leds.SetMode(ModeIdle)
			audio.SetPaused(false)
		}()
	}
}

func speakAnswer(answer string, leds *LEDManager, player *AudioPlayer, audio *AudioCapture, tts *TTSClient) {
	leds.SetMode(ModeSpeaking)
	outputWAV := "/tmp/tts_response.wav"
	currentVol := player.GetVolume()
	start := time.Now()
	if err := tts.SynthesizeToWAV(answer, outputWAV, currentVol); err != nil {
		log.Printf("[TTS] Synthesis error: %v", err)
		return
	}
	log.Printf("[TTS] Synthesized in %v: \"%s\"", time.Since(start), answer)
	audio.SetPaused(true)
	_ = player.Play(outputWAV)
	time.Sleep(50 * time.Millisecond)
}
