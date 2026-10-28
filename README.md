# Open Station Assistant

> **Autonomous, privacy-first smart speaker assistant running natively on the Yandex Station Mini 1 (Amlogic A113X).**

---

> [!NOTE]
> **Project Attribution & Origin:**
> This entire project — including hardware reverse engineering, ALSA audio pipeline, Rust ONNX wake-word spotter, Go daemon architecture, and peripheral drivers — was researched, designed, and written collaboratively with **Google DeepMind's Antigravity AI coding assistant agent**.

---

## Overview

Open Station Assistant turns a rooted **Yandex Station Mini 1** (Linkplay A98 / Amlogic A113X SoC) into a fully open, autonomous smart speaker. It replaces proprietary cloud daemons with an independent on-device pipeline:

```
[ MEMS Microphones (Ch 0+1) ]
           │ (48kHz S16_LE)
           ▼
[ 150Hz HPF + 3.0x Preamp ] ──► [ 16kHz Downsampler ]
                                       │
                                       ▼
                     [ Rust openWakeWord Spotter (tract-onnx) ]
                                       │
                       (Local Wake Detected: "Jarvis" / "Alexa")
                                       │
                                       ▼
                       [ Instant Hardware Chime (<50ms) ]
                                       │
                       [ VAD Dynamic Speech Capture ]
                                       │
                     (Audio Question WAV sent over TLS)
                                       │
                                       ▼
                       [ Cloud STT: Whisper Large v3 ]
                                       │
                                       ▼
                         [ LLM: Qwen / LLaMA on Groq ]
                                       │
                                       ▼
                    [ Local TTS: RHVoice (Anna / Elena) ]
                                       │
                                       ▼
                     [ Internal DAC / Amp (ALSA hw:0,0) ]
```

---

## Key Features

1. **Strictly Local Wake Word (Zero Idle Traffic)**:
   - Wake word detection runs 100% locally on the speaker using a custom Rust engine powered by `tract-onnx`.
   - Bundled models: `alexa.onnx`, `hey_jarvis_v0.1.onnx`, `hey_mycroft_v0.1.onnx`, `ahoj_hugo.onnx`.
   - Sensitivity calibrated to `0.20` threshold for reliable single-word invocation (*"Jarvis"*, *"Джарвис"*, *"Alexa"*).
   - Network bandwidth during listening is strictly **0 bytes/second**.
   - Negligible CPU footprint (~12% of a single core; overall system 85% idle).

2. **Instant Response Latency & Universal Stop Chime**:
   - Audio chime triggers directly on-device in **< 50ms**, providing instant auditory feedback.
   - **Universal Stop Chime**: Plays the authentic descending cancel chime (`spotter_cancel.mp3`) whenever the speaker stops listening:
     - On user stop words (*"стоп"*, *"хватит"*, *"спасибо"*, *"отмена"*, *"выключи"*);
     - On conversation or follow-up timeout (no speech detected);
     - On hand-over-sensor gesture.

3. **On-Device Human TTS**:
   - Speech synthesis runs locally on the speaker using a patched build of **RHVoice** (Anna and Elena 24kHz natural Russian voices).
   - Audio is upsampled linearly to 48kHz stereo to match the hardware DAC without popping or resampling distortion.

4. **Calibrated MEMS Microphone Pipeline**:
   - Hardware ALSA device `hw:0,2` provides 4 TDM slots. Channels 0 & 1 are the physical MEMS microphones, while Channels 2 & 3 are hardware decimator/reference lanes.
   - Real-time `(Ch0 + Ch1) / 2.0` averaging provides 360° pickup and +3 dB SNR gain.
   - Built-in 150 Hz IIR High-Pass Filter ($\alpha = 0.9807$) eliminates ambient room rumble and fan hum.
   - Calibrated +9.5 dB ($3.0\times$) digital preamp staging ensures acoustic features match openWakeWord's training distribution from across the room without clipping or noise elevation.

5. **Hardware Peripheral Drivers & Gestures**:
   - **LED Ring**: Direct I2C driver on `/dev/i2c-0` (`0x3c`) controlling the 12 RGB LEDs (36 PWM channels):
     - *Standby (Idle)*: Completely OFF (0% light/power).
     - *Startup Flash*: Vibrant purple ring pulse on boot confirming system readiness.
     - *Listening*: Smooth sinusoidal breathing purple (2.0s period).
     - *Thinking*: Rotating cyan-blue dot with a 4-LED decaying trail.
     - *Speaking*: Dynamic voice VU meter pulsing brightness in sync with speech waveforms.
     - *Volume Adjustment*: Proportional radial arc (1–12 LEDs) with continuous gradient shifting (Cyan $\to$ Green $\to$ Orange $\to$ Crimson).
   - **Intelligent ToF Gestures (ST VL53L0X)**:
     - *Authentic Dwell Delay*: Requires 300ms of stable hand position in the detection zone (65–340mm) before volume mode activates, preventing accidental volume jumps when passing nearby.
     - *Discrete 10-Step Tracking (Zero Lag)*: Once activated, vertical hand motion changes volume across 10 discrete levels (10%–100%) with an instant click sound and LED arc update per step.
     - *Stationary Obstacle Rejection*: Static surfaces (shelves, ceilings, resting obstacles with $\le 12$mm variance for $> 1.0$s) are automatically identified and ignored as background.
     - *Palm Stop*: Covering the sensor ($< 42$mm) immediately stops all speech/music/processing, sounds the cancel chime, and returns directly to standby wake-word listening.

6. **Continuous Conversation (Follow-Up Mode)**:
   - After answering a question, the assistant stays in active follow-up listening mode for 5.5 seconds (breathing purple LED).
   - You can speak follow-up inquiries naturally without repeating the wake word.
   - Saying *"Спасибо"*, *"Стоп"*, or allowing the timeout to expire sounds the stop chime and smoothly returns the unit to standby.

---

## Repository Structure

```text
├── daemon/                     # Go Assistant Daemon
│   ├── ai.go                   # Whisper STT & LLM chat completions
│   ├── audio.go                # ALSA capture, 150Hz HPF, 3x preamp, VAD
│   ├── config.go               # Configuration structure & defaults
│   ├── intents.go              # Intent router (weather, music, time, volume, stop)
│   ├── led.go                  # TLC59116 I2C RGB ring controller
│   ├── main.go                 # Event loop, cancellation & process supervisor
│   ├── music.go                # Music streaming service integration
│   ├── player.go               # Tinyplay wrapper, non-blocking click & digital staging
│   ├── spotter.go              # IPC manager for local spotter
│   ├── tof.go                  # VL53L0X gesture manager (discrete 10-step & dwell)
│   ├── tts.go                  # RHVoice on-device synthesis client
│   └── weather.go              # Open-Meteo weather client
├── spotter/                    # Rust Local Wake Word Engine (openWakeWord)
│   ├── Cargo.toml
│   ├── src/main.rs             # tract-onnx acoustic inference engine
│   ├── models/                 # Spectrogram and embedding ONNX models
│   └── speech_models/          # Wake word classifier heads
├── tts/                        # RHVoice Standalone Integration
│   └── rhvoice_synth.c         # C wrapper with 48kHz stereo upsampler
├── scripts/
│   ├── build.sh                # Cross-compilation build helper
│   ├── start_services.sh       # Service startup helper
│   └── S99station-assistant    # /etc/init.d/ on-boot autostart service script
├── config.example.json         # Example configuration file
├── .gitignore
├── LICENSE
└── README.md
```

---

## Configuration

Copy `config.example.json` to `/bkupgrade/data/config.json` on the device and populate your API keys:

```json
{
  "groq_api_key": "YOUR_GROQ_API_KEY",
  "groq_base_url": "https://api.groq.com/openai/v1",
  "stt_model": "whisper-large-v3-turbo",
  "llm_model": "qwen/qwen3.8-27b",
  "tts_voice": "Anna",
  "tts_speed": 100,
  "tts_pitch": 100,
  "default_volume": 15,
  "wake_words": ["jarvis", "alexa"],
  "local_wakeword_model": "alexa,jarvis",
  "wakeword_threshold": 0.20,
  "sensor_enabled": true,
  "city": "Moscow",
  "latitude": 55.7558,
  "longitude": 37.6173,
  "timezone_offset_hours": 3,
  "byedpi_enabled": false,
  "byedpi_port": 1080
}
```

### Network & Region Bypass

In regions where cloud AI endpoints require proxy routing, you can run a local proxy client (e.g. `sing-box`, `byedpi`, or custom tunnels using protocols such as VLESS, SOCKS5, or Shadowsocks) bound to `127.0.0.1:1080`. 

The Go daemon automatically checks for a local SOCKS5 proxy on `127.0.0.1:1080` and routes outbound HTTPS traffic through it transparently if available.

---

## Autostart on Boot

To run the assistant automatically on device boot, install the init script:

```bash
cp scripts/S99station-assistant /etc/init.d/S99station-assistant
chmod +x /etc/init.d/S99station-assistant
```

---

## Building from Source

### Prerequisites

- Go 1.21+ (with `GOOS=linux GOARCH=arm64` cross-compilation support)
- Rust toolchain with `aarch64-unknown-linux-musl` target:
  ```bash
  rustup target add aarch64-unknown-linux-musl
  ```
- AArch64 GCC cross-compiler (`aarch64-linux-gnu-gcc`)

### Compilation

Run the build helper:

```bash
chmod +x scripts/build.sh
./scripts/build.sh
```

Or build components individually:

1. **Build Spotter (Rust)**:
   ```bash
   cd spotter
   cargo build --target aarch64-unknown-linux-musl --release
   aarch64-linux-gnu-strip target/aarch64-unknown-linux-musl/release/station-spotter
   ```

2. **Build Daemon (Go)**:
   ```bash
   cd daemon
   GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o station-assistant .
   ```

3. **Build RHVoice Wrapper (C)**:
   ```bash
   cd tts
   aarch64-linux-gnu-gcc -O2 -s -o rhvoice_synth rhvoice_synth.c -ldl -lm
   ```

---

## License

This project is licensed under the [MIT License](LICENSE).
