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
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/projectionist ./cmd/projectionist \
 && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o /out/subocr ./cmd/subocr \
 && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o /out/subvad ./cmd/subvad

# The same image tag the NAS runs, so the ffmpeg we exec is byte-identical to Jellyfin's (QSV/VAAPI, iHD driver).
FROM ${JELLYFIN_IMAGE} AS jellyfin

# alass aligns a subtitle to a film's speech (the subtitles stage, when no English image track gives the timing).
# Pinned to the last upstream commit: its --index flag (which audio stream) is missing from the 2019 release binary.
# Alpine's musl toolchain links it statically.
FROM --platform=linux/amd64 rust:1.96-alpine AS alass
RUN apk add --no-cache git musl-dev
RUN cargo install --locked --root /out --git https://github.com/kaegi/alass --rev 874f02d9577182752a0f969b6d6b98fd65bdf1fc alass-cli

# subocr reads image subtitles for the subtitles stage (its OCR pass): ONNX Runtime's CPU build, which subocr loads at
# run time, and PP-OCRv6 small's text-line recognition model as RapidOCR exports it (the dictionary in its metadata).
FROM --platform=$BUILDPLATFORM debian:trixie-slim AS ocr
ADD --checksum=sha256:a5ed5a3cac51fbb2e90da632ae43d19212faaa20e76484e62bcb7c23ddb3b3fd \
    https://github.com/microsoft/onnxruntime/releases/download/v1.30.0/onnxruntime-linux-x64-1.30.0.tgz /ort.tgz
ADD --chmod=644 --checksum=sha256:6f327246b50388f3c176ae304bd95767ea6dc0c9ae92153ef8cbe210b3c14884 \
    https://www.modelscope.cn/models/RapidAI/RapidOCR/resolve/v3.9.2/onnx/PP-OCRv6/rec/PP-OCRv6_rec_small.onnx /out/PP-OCRv6_rec_small.onnx
# subvad hears speech for the subtitles stage's alignments with Silero VAD v6.2 on the same ONNX Runtime.
ADD --chmod=644 --checksum=sha256:1a153a22f4509e292a94e67d6f9b85e8deb25b4988682b7e174c65279d8788e3 \
    https://raw.githubusercontent.com/snakers4/silero-vad/v6.2/src/silero_vad/data/silero_vad.onnx /out/silero_vad.onnx
RUN tar -xzf /ort.tgz -C / && cp /onnxruntime-linux-x64-1.30.0/lib/libonnxruntime.so.1.30.0 /out/

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
COPY --from=ocr /out/libonnxruntime.so.1.30.0 /usr/local/lib/libonnxruntime.so
COPY --from=ocr /out/PP-OCRv6_rec_small.onnx /usr/local/share/subocr/PP-OCRv6_rec_small.onnx
COPY --from=ocr /out/silero_vad.onnx /usr/local/share/subvad/silero_vad.onnx
COPY --from=build /out/subocr /usr/local/bin/subocr
COPY --from=build /out/subvad /usr/local/bin/subvad
COPY --from=build /out/projectionist /usr/local/bin/projectionist
ENTRYPOINT ["/usr/local/bin/projectionist"]
CMD ["serve", "-config", "/config/projectionist.toml"]
