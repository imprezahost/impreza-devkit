#!/usr/bin/env bash
# Impreza API — quickstart with curl.
#
# Set credentials in env:
#   export IMPREZA_API_KEY="imp_..."
#   export IMPREZA_API_SECRET="..."

set -euo pipefail

: "${IMPREZA_API_KEY:?Set IMPREZA_API_KEY first}"
: "${IMPREZA_API_SECRET:?Set IMPREZA_API_SECRET first}"

BASE="${IMPREZA_API_BASE:-https://api.imprezahost.com/v1}"

# 1) Who am I (key identity, IP whitelist, rate limit)
curl -s "$BASE/account/api-keys/self" \
    -H "X-API-Key: $IMPREZA_API_KEY" \
    -H "X-API-Secret: $IMPREZA_API_SECRET" | jq

# 2) Account balance
curl -s "$BASE/account" \
    -H "X-API-Key: $IMPREZA_API_KEY" \
    -H "X-API-Secret: $IMPREZA_API_SECRET" | jq '.data | {balance, currency}'

# 3) Crypto top-up (Monero)
curl -s -X POST "$BASE/account/topup" \
    -H "X-API-Key: $IMPREZA_API_KEY" \
    -H "X-API-Secret: $IMPREZA_API_SECRET" \
    -H "Content-Type: application/json" \
    -d '{"amount": 50, "method": "xmr"}' | jq

# 4) Subscribe to events
curl -s -X POST "$BASE/webhooks" \
    -H "X-API-Key: $IMPREZA_API_KEY" \
    -H "X-API-Secret: $IMPREZA_API_SECRET" \
    -H "Content-Type: application/json" \
    -d '{
        "url": "https://example.com/hooks/impreza",
        "events": ["topup.paid", "vps.*"],
        "description": "production handler"
    }' | jq
