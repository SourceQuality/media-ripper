# syntax=docker/dockerfile:1
#
# media-ripper image: the Go daemon plus makemkvcon, ffmpeg and mkvtoolnix.
# MakeMKV is built from source because its license forbids redistribution of
# the binary; pin the version with --build-arg MAKEMKV_VERSION=x.y.z.

ARG MAKEMKV_VERSION=1.17.9

# ---- Go binary -------------------------------------------------------------
FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=docker
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/media-ripper ./cmd/media-ripper

# ---- MakeMKV ---------------------------------------------------------------
FROM debian:bookworm-slim AS makemkv
ARG MAKEMKV_VERSION
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      build-essential pkg-config ca-certificates wget \
      libc6-dev libssl-dev libexpat1-dev libavcodec-dev libgl1-mesa-dev zlib1g-dev \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /build
RUN wget -q https://www.makemkv.com/download/makemkv-oss-${MAKEMKV_VERSION}.tar.gz \
 && wget -q https://www.makemkv.com/download/makemkv-bin-${MAKEMKV_VERSION}.tar.gz \
 && tar xzf makemkv-oss-${MAKEMKV_VERSION}.tar.gz \
 && tar xzf makemkv-bin-${MAKEMKV_VERSION}.tar.gz
RUN cd makemkv-oss-${MAKEMKV_VERSION} && ./configure --disable-gui && make -j"$(nproc)" && make install
RUN cd makemkv-bin-${MAKEMKV_VERSION} && mkdir -p tmp && echo accepted > tmp/eula_accepted && make && make install

# ---- Runtime ---------------------------------------------------------------
FROM debian:bookworm-slim
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ffmpeg mkvtoolnix eject tesseract-ocr tesseract-ocr-eng libssl3 libexpat1 libavcodec59 zlib1g tini \
    && rm -rf /var/lib/apt/lists/*
# makemkv-oss and makemkv-bin install only into /usr/local (bin, lib, share).
COPY --from=makemkv /usr/local/ /usr/local/
RUN ldconfig
COPY --from=build /out/media-ripper /usr/local/bin/media-ripper
COPY deploy/entrypoint.sh /entrypoint.sh
COPY config.example.yaml /config.example.yaml
RUN chmod +x /entrypoint.sh && mkdir -p /config /output /workspace
ENV MR_CONFIG=/config/config.yaml \
    MR_OUTPUT_PATH=/output \
    MR_WORKSPACE=/workspace \
    HOME=/config
VOLUME ["/config", "/output", "/workspace"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/usr/bin/tini", "--", "/entrypoint.sh"]
CMD ["run"]
