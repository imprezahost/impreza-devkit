#!/bin/sh
# Run after package install / upgrade. We deliberately do NOT enable
# or start the unit here — bootstrap must complete first (the operator
# runs `impreza-agent bootstrap` and then `systemctl enable --now`).
set -eu

systemctl daemon-reload || true

cat <<MSG

impreza-agent installed.

Next steps:
  1. Issue a bootstrap token from the panel.
  2. sudo impreza-agent bootstrap --token bst_xxxxxxxxxxxxxxxx
  3. sudo systemctl enable --now impreza-agent

Diagnostics:
  sudo impreza-agent doctor
  journalctl -u impreza-agent -f

MSG
