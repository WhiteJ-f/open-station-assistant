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
[ MEMS Microphones (Ch 2+3) ]
           │ (48kHz S16_LE)
           ▼
[ 150Hz HPF + 12x Preamp ] ──► [ 16kHz Downsampler ]
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
   - Network bandwidth during listening is strictly **0 bytes/second**.
   - Negligible CPU footprint (~12% of a single core; overall system 85% idle).

2. **Instant Response Latency**:
   - Audio chime triggers directly on-device in **< 50ms**, providing instant auditory feedback identical to factory devices.

3. **On-Device Human TTS**:
   - Speech synthesis runs locally on the speaker using a patched build of **RHVoice** (Anna and Elena 24kHz natural Russian voices).
   - Audio is upsampled linearly to 48kHz stereo to match the hardware DAC without popping or resampling distortion.

4. **Calibrated MEMS Microphone Pipeline**:
   - Hardware ALSA device `hw:0,2` provides 4 TDM slots. Slots 0 & 1 contain zero-padding, while Slots 2 & 3 are physical MEMS microphones.
   - Real-time `(Ch2 + Ch3) / 2.0` averaging provides 360° pickup and +3 dB SNR gain.
   - Built-in 150 Hz IIR High-Pass Filter ($\alpha = 0.9807$) eliminates ambient room rumble and fan hum.
   - Calibrated +21.6 dB ($12.0\times$) digital preamp staging ensures acoustic features match openWakeWord's training distribution from across the room.

5. **Hardware Peripheral Drivers**:
   - **LED Ring**: Direct I2C control of the onboard NXP TLC59116 16-channel LED driver on `i2c-0` (`0x68`) with smooth state animations (idle, listening, thinking, speaking).
   - **Time-of-Flight Gesture Sensor**: STMicroelectronics VL53L0X laser distance sensor on `/dev/input/event0` for proximity and hover volume gestures.

---

## Repository Structure

```text
├── daemon/                     # Go Assistant Daemon
│   ├── ai.go                   # Whisper STT & LLM chat completions
│   ├── audio.go                # ALSA capture, 150Hz HPF, 12x preamp, VAD
│   ├── config.go               # Configuration structure & defaults
│   ├── intents.go              # Intent router (weather, music, time, volume)
│   ├── led.go                  # TLC59116 I2C RGB ring controller
│   ├── main.go                 # Event loop & process supervisor
│   ├── music.go                # Music streaming service integration
│   ├── player.go               # Tinyplay wrapper & digital volume staging
│   ├── spotter.go              # IPC manager for local spotter
│   ├── tof.go                  # VL53L0X gesture manager
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
│   └── start_services.sh       # On-boot service supervisor
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
  "wakeword_threshold": 0.35,
  "sensor_enabled": false,
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
