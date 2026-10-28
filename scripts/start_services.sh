#!/bin/sh
# Standalone smart assistant supervisor for Yandex Station Mini
# Place in /bkupgrade/bin/start_services.sh

CONFIG="/bkupgrade/data/config.json"
BIN_DIR="/bkupgrade/bin"

# 1. Optional SOCKS5 proxy (if configured for region bypass)
if [ -x "$BIN_DIR/sing-box" ] && [ -f "/bkupgrade/data/singbox.json" ]; then
    if ! killall -0 sing-box 2>/dev/null; then
        /usr/bin/nohup "$BIN_DIR/sing-box" run -c /bkupgrade/data/singbox.json > /tmp/singbox.log 2>&1 &
        sleep 1
    fi
fi

# 2. Start station-assistant daemon if not running
if ! killall -0 station-assistant 2>/dev/null; then
    /usr/bin/nohup "$BIN_DIR/station-assistant" "$CONFIG" > /tmp/assistant.log 2>&1 &
    sleep 1
fi
