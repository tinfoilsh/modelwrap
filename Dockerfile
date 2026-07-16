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

RUN printf '%s\n' \
    "deb [check-valid-until=no] https://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT}/ trixie main" \
    "deb [check-valid-until=no] https://snapshot.debian.org/archive/debian-security/${DEBIAN_SNAPSHOT}/ trixie-security main" \
    "deb [check-valid-until=no] https://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT}/ bookworm main" \
    > /etc/apt/sources.list && \
    rm -f /etc/apt/sources.list.d/*.sources && \
    apt-get update && \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        erofs-utils=1.5-1 \
        cryptsetup=2:2.7.5-2 \
        gdisk=1.0.10-2 && \
    rm -rf /var/lib/apt/lists/*

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
