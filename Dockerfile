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

FROM debian:trixie-slim
# jellyfin-ffmpeg8's runtime dependencies (`apt-cache depends jellyfin-ffmpeg8` inside the Jellyfin image).
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
      libbluray2 libbz2-1.0 libelf1t64 libexpat1 libgmp10 libgnutls30t64 libmp3lame0 libopenmpt0t64 libopus0 \
      libpciaccess0 libstdc++6 libudev1 libvorbis0a libvorbisenc2 libvpx9 libwebp7 libwebpmux3 libx264-164 \
      libx265-215 libzstd1 libzvbi0t64 ocl-icd-libopencl1 \
 && rm -rf /var/lib/apt/lists/*
COPY --from=jellyfin /usr/lib/jellyfin-ffmpeg /usr/lib/jellyfin-ffmpeg
COPY --from=build /out/projectionist /usr/local/bin/projectionist
ENTRYPOINT ["/usr/local/bin/projectionist"]
CMD ["serve", "-config", "/config/projectionist.toml"]
