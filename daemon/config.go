package main

import (
	"encoding/json"
	"os"
)

type Config struct {
	GroqAPIKey          string   `json:"groq_api_key"`
	GroqBaseURL         string   `json:"groq_base_url"`
	STTModel            string   `json:"stt_model"`
	LLMModel            string   `json:"llm_model"`
	TTSVoice            string   `json:"tts_voice"`
	TTSSpeed            int      `json:"tts_speed"`
	TTSPitch            int      `json:"tts_pitch"`
	DefaultVolume       int      `json:"default_volume"`
	ChimeSound          string   `json:"chime_sound"`
	CancelSound         string   `json:"cancel_sound"`
	VolumeSound         string   `json:"volume_sound"`
	WakeWords           []string `json:"wake_words"`
	LocalWakeWordModel  string   `json:"local_wakeword_model"`
	WakeWordThreshold   float64  `json:"wakeword_threshold"`
	SensorEnabled       bool     `json:"sensor_enabled"`
	City                string   `json:"city"`
	Latitude            float64  `json:"latitude"`
	Longitude           float64  `json:"longitude"`
	TimezoneOffsetHours int      `json:"timezone_offset_hours"`
	ByeDPIEnabled       bool     `json:"byedpi_enabled"`
	ByeDPIPort          int      `json:"byedpi_port"`
}

func DefaultConfig() *Config {
	return &Config{
		GroqAPIKey:          "",
		GroqBaseURL:         "https://api.groq.com/openai/v1",
		STTModel:            "whisper-large-v3-turbo",
		LLMModel:            "qwen/qwen3.8-27b",
		TTSVoice:            "Anna",
		TTSSpeed:            100,
		TTSPitch:            100,
		DefaultVolume:       15,
		ChimeSound:          "/bkupgrade/data/chime.wav",
		CancelSound:         "/bkupgrade/data/cancel.wav",
		VolumeSound:         "/bkupgrade/data/volume.wav",
		WakeWords:           []string{"jarvis", "alexa"},
		LocalWakeWordModel:  "alexa,jarvis",
		WakeWordThreshold:   0.35,
		SensorEnabled:       false,
		City:                "Moscow",
		Latitude:            55.7558,
		Longitude:           37.6173,
		TimezoneOffsetHours: 3, // UTC+3
		ByeDPIEnabled:       false,
		ByeDPIPort:          1080,
	}
}

func LoadConfig(path string) *Config {
	cfg := DefaultConfig()
	data, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(data, cfg)
	}
	return cfg
}
