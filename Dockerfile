# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
# Build stage runs the tests with real ffmpeg (libx264: CI has no iGPU), so a
# failing test or conversion fails the image build.
FROM golang:1.27-alpine@sha256:b8c9b82affbbb82a020bbcf998dad2ba9516944ac7e883524642a481a9505e9c AS build
RUN apk add --no-cache ffmpeg
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY web/ web/
ARG VERSION=dev
ENV SHARE_TEST_ENCODER=libx264 SHARE_REQUIRE_FFMPEG=1
RUN go vet ./... && go test ./... && \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /grab .

# Not scratch: conversion needs ffmpeg, VAAPI on an Intel iGPU needs Intel's
# iHD driver, and notifications shell out to the Apprise CLI (a Python
# package, pinned in requirements.txt and installed in its own venv).
# `apk upgrade` keeps a Trivy gate on fixable CRITICAL/HIGH green.
FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
COPY requirements.txt /tmp/requirements.txt
RUN apk --no-cache upgrade && apk add --no-cache ffmpeg intel-media-driver python3 && \
    python3 -m venv /opt/apprise && \
    /opt/apprise/bin/pip install --no-cache-dir -r /tmp/requirements.txt && \
    # pip is only the installer: drop it so the image doesn't ship pip's own
    # vendored libraries (urllib3, msgpack, ...) for scanners to flag.
    /opt/apprise/bin/python -m pip uninstall -y -q pip && \
    rm /tmp/requirements.txt && \
    mkdir /data /shares /cookies && chown 65532:65532 /data /shares /cookies
COPY --from=build /grab /usr/local/bin/grab
# The source label links the published package to its repository.
LABEL org.opencontainers.image.source="https://github.com/beefstew809/grab" \
      org.opencontainers.image.licenses="AGPL-3.0-only"
USER 65532:65532
EXPOSE 8080
ENV LIBVA_DRIVER_NAME=iHD PATH=/opt/apprise/bin:$PATH
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s CMD ["grab", "-healthcheck"]
ENTRYPOINT ["grab"]
