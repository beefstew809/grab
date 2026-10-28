#!/bin/sh
# Browser checks of the PWA against a MOCKED /api (Playwright routes), served
# by python's http.server from web/. Nothing installed on the host.
set -e
cd "$(dirname "$0")"
img=mcr.microsoft.com/playwright/python:v1.63.0-noble
podman run --rm -v "$PWD":/out:z -v "$PWD/../web":/web:ro,z "$img" \
  sh -c "pip install -q --break-system-packages --root-user-action=ignore playwright==1.63.0 && \
         (python3 -m http.server 18081 -d /web >/dev/null 2>&1 &) && sleep 1 && python3 /out/share.py"
