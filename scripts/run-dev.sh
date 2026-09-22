#!/usr/bin/env bash
set -euo pipefail
exec go run ./cmd/gateway -config configs/config.example.json
