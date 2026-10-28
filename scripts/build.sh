#!/usr/bin/env bash
set -euo pipefail

# Build script for Yandex Station Mini (Amlogic A113X, Linux aarch64)

echo "=== Building Rust Local Spotter (tract-onnx, static musl) ==="
cd spotter
cargo build --target aarch64-unknown-linux-musl --release
aarch64-linux-gnu-strip target/aarch64-unknown-linux-musl/release/station-spotter
cd ..

echo "=== Building Go Assistant Daemon ==="
cd daemon
GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o station-assistant .
cd ..

echo "=== Building RHVoice C Wrapper ==="
cd tts
aarch64-linux-gnu-gcc -O2 -s -o rhvoice_synth rhvoice_synth.c -ldl -lm
cd ..

echo "=== Build Complete! ==="
echo "Binaries ready:"
echo " - spotter/target/aarch64-unknown-linux-musl/release/station-spotter"
echo " - daemon/station-assistant"
echo " - tts/rhvoice_synth"
