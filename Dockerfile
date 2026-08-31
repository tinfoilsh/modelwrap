FROM golang:1.25-trixie@sha256:3140b898a3ec52ec5e8a7dc325a3dbdc732c35e0bde3fcc0e0d764c781d7da10 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY crypt ./crypt
COPY wrap ./wrap
COPY cmd ./cmd
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags=-buildid= -o /modelwrap ./cmd/modelwrap

FROM python:3.13-slim-trixie@sha256:b04b5d7233d2ad9c379e22ea8927cd1378cd15c60d4ef876c065b25ea8fb3bf3

ARG DEBIAN_SNAPSHOT=20260518T000000Z

# Each pack schema vendors its own pinned mkfs.erofs (see schema.go):
# schema 1 = erofs-utils 1.5-1 (bookworm), schema 2 = 1.8.6-1 (trixie,
# multithreaded). Both debs come from the dated snapshot archive and are
# additionally sha256-pinned (amd64: pack reproducibility is defined for
# the published amd64 image).
RUN printf '%s\n' \
    "deb [check-valid-until=no] https://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT}/ trixie main" \
    "deb [check-valid-until=no] https://snapshot.debian.org/archive/debian-security/${DEBIAN_SNAPSHOT}/ trixie-security main" \
    "deb [check-valid-until=no] https://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT}/ bookworm main" \
    > /etc/apt/sources.list && \
    rm -f /etc/apt/sources.list.d/*.sources && \
    apt-get update && \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        cryptsetup=2:2.7.5-2 \
        gdisk=1.0.10-2 \
        libdeflate0 liblz4-1 liblzma5 libselinux1 libuuid1 libxxhash0 libzstd1 zlib1g && \
    cd /tmp && \
    apt-get download erofs-utils=1.5-1 && \
    apt-get download erofs-utils=1.8.6-1 && \
    printf '%s\n' \
        "902aa0a3791f4dc86f223359141b0b39991e432b95687873502587329b8e843d  erofs-utils_1.5-1_amd64.deb" \
        "81042b8c0c3eb63699aa7d0763dceedc675a747d9f5cfebf914b4160233f411e  erofs-utils_1.8.6-1_amd64.deb" \
        | sha256sum -c && \
    dpkg-deb -x erofs-utils_1.5-1_amd64.deb /tmp/erofs-v1 && \
    dpkg-deb -x erofs-utils_1.8.6-1_amd64.deb /tmp/erofs-v2 && \
    install -D /tmp/erofs-v1/usr/bin/mkfs.erofs /opt/modelwrap/schemas/v1/mkfs.erofs && \
    install -D /tmp/erofs-v2/usr/bin/mkfs.erofs /opt/modelwrap/schemas/v2/mkfs.erofs && \
    /opt/modelwrap/schemas/v1/mkfs.erofs 2>&1 | grep -q '^mkfs.erofs 1.5$' && \
    /opt/modelwrap/schemas/v2/mkfs.erofs -V 2>&1 | grep -q '(erofs-utils) 1.8.6' && \
    /opt/modelwrap/schemas/v2/mkfs.erofs --help 2>&1 | grep -q -- --workers && \
    rm -rf /tmp/erofs-v1 /tmp/erofs-v2 /tmp/*.deb /var/lib/apt/lists/*

WORKDIR /app
COPY requirements.txt .

ENV CACHE_DIR="/cache"
ENV OUTPUT_DIR="/output"
# Marks the packing context so the CLI runs directly instead of
# re-launching itself in a container.
ENV MODELWRAP_IN_CONTAINER=1

# huggingface_hub provides the `hf` CLI used for model downloads.
RUN pip install --no-cache-dir --require-hashes -r requirements.txt

COPY --from=build /modelwrap /usr/local/bin/modelwrap

ENTRYPOINT ["modelwrap"]
