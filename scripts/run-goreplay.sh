#!/usr/bin/env bash
set -euo pipefail

# Capture traffic inside nginx-love-backend network namespace (port 80)
# Forward to http-ingest on host (9002)
sudo docker run --rm \
  --net=container:nginx-love-backend \
  --cap-add=NET_RAW --cap-add=NET_ADMIN \
  buger/goreplay \
  --input-raw :80 \
  --http-original-host \
  --output-http "http://host.docker.internal:9002" \
  --verbose
