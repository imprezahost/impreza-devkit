#!/bin/sh
# Run before package removal. Stops the running service so the binary
# can be replaced cleanly. Does NOT delete /etc/impreza-agent or
# /var/lib/impreza-agent — credentials and state survive uninstall so
# a reinstall doesn't require re-bootstrapping.
set -eu

if systemctl is-active --quiet impreza-agent; then
    systemctl stop impreza-agent || true
fi
if systemctl is-enabled --quiet impreza-agent 2>/dev/null; then
    systemctl disable impreza-agent || true
fi
