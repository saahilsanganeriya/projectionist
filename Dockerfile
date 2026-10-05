# syntax=docker/dockerfile:1
# Build:  make image   (linux/amd64; the Go stage cross-compiles natively, only the runtime stage is emulated)
ARG JELLYFIN_IMAGE=jellyfin/jellyfin:12.1.20260915-010956

FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/projectionist ./cmd/projectionist

# The same image tag the NAS runs, so the ffmpeg we exec is byte-identical to Jellyfin's (QSV/VAAPI, iHD driver).
FROM ${JELLYFIN_IMAGE} AS jellyfin

# alass aligns a subtitle to a film's speech (the subtitles stage, when no English image track gives the timing).
# Pinned to the last upstream commit: its --index flag (which audio stream) is missing from the 2019 release binary.
# Alpine's musl toolchain links it statically.
FROM --platform=linux/amd64 rust:1.96-alpine AS alass
RUN apk add --no-cache git musl-dev
RUN cargo install --locked --root /out --git https://github.com/kaegi/alass --rev 874f02d9577182752a0f969b6d6b98fd65bdf1fc alass-cli

FROM debian:trixie-slim
# jellyfin-ffmpeg8's runtime dependencies (`apt-cache depends jellyfin-ffmpeg8` inside the Jellyfin image).
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
      libbluray2 libbz2-1.0 libelf1t64 libexpat1 libgmp10 libgnutls30t64 libmp3lame0 libopenmpt0t64 libopus0 \
      libpciaccess0 libstdc++6 libudev1 libvorbis0a libvorbisenc2 libvpx9 libwebp7 libwebpmux3 libx264-164 \
      libx265-215 libzstd1 libzvbi0t64 ocl-icd-libopencl1 \
 && rm -rf /var/lib/apt/lists/*
COPY --from=jellyfin /usr/lib/jellyfin-ffmpeg /usr/lib/jellyfin-ffmpeg
COPY --from=alass /out/bin/alass-cli /usr/local/bin/alass
COPY --from=build /out/projectionist /usr/local/bin/projectionist
ENTRYPOINT ["/usr/local/bin/projectionist"]
CMD ["serve", "-config", "/config/projectionist.toml"]
