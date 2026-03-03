#!/bin/bash
set -euo pipefail

echo "[pai-bridge] Starting entrypoint..."

# Ensure PVC directories exist
mkdir -p /mnt/pai-data/claude \
         /mnt/pai-data/memory \
         /mnt/pai-data/projects \
         /mnt/pai-data/claude/skills/TelegramBridge/state

# Symlink Claude home dirs to PVC
rm -rf /home/claude/.claude /home/claude/projects
ln -sf /mnt/pai-data/claude /home/claude/.claude
ln -sf /mnt/pai-data/projects /home/claude/projects

# Ensure onboarding bypass
if [ ! -f /home/claude/.claude.json ]; then
  echo '{"hasCompletedOnboarding":true}' > /home/claude/.claude.json
fi
chown claude:claude /home/claude/.claude.json

# Merge ConfigMap settings with PVC settings
if [ -f /etc/pai-bridge/settings.json ]; then
  if [ -f /mnt/pai-data/claude/settings.json ]; then
    echo "[pai-bridge] Merging ConfigMap settings with existing PVC settings..."
    jq -s '.[0] * .[1]' /mnt/pai-data/claude/settings.json /etc/pai-bridge/settings.json > /tmp/merged-settings.json
    mv /tmp/merged-settings.json /mnt/pai-data/claude/settings.json
  else
    echo "[pai-bridge] First boot: copying ConfigMap settings to PVC..."
    cp /etc/pai-bridge/settings.json /mnt/pai-data/claude/settings.json
  fi
fi

# Inject secrets from env vars into settings.json
SETTINGS="/mnt/pai-data/claude/settings.json"
if [ -f "$SETTINGS" ]; then
  if [ -n "${TELEGRAM_BOT_TOKEN:-}" ]; then
    echo "[pai-bridge] Injecting TELEGRAM_BOT_TOKEN into settings..."
    jq --arg token "$TELEGRAM_BOT_TOKEN" '.env.TELEGRAM_BOT_TOKEN = $token' "$SETTINGS" > /tmp/s.json && mv /tmp/s.json "$SETTINGS"
  fi
  if [ -n "${ELEVENLABS_API_KEY:-}" ]; then
    jq --arg key "$ELEVENLABS_API_KEY" '.env.ELEVENLABS_API_KEY = $key' "$SETTINGS" > /tmp/s.json && mv /tmp/s.json "$SETTINGS"
  fi
  if [ -n "${TELEGRAM_ALLOWED_USERS:-}" ]; then
    echo "[pai-bridge] Injecting TELEGRAM_ALLOWED_USERS into settings..."
    jq --arg users "$TELEGRAM_ALLOWED_USERS" '.telegramBridge.allowed_users = ($users | split(","))' "$SETTINGS" > /tmp/s.json && mv /tmp/s.json "$SETTINGS"
  fi
fi

# Fix ownership
chown -R claude:claude /mnt/pai-data /home/claude

echo "[pai-bridge] Starting bridge..."
exec /usr/local/bin/pai-bridge
