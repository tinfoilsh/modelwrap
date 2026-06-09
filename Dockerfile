FROM python:3.13-slim-trixie@sha256:b04b5d7233d2ad9c379e22ea8927cd1378cd15c60d4ef876c065b25ea8fb3bf3

ARG DEBIAN_SNAPSHOT=20260518T000000Z

RUN printf '%s\n' \
    "deb [check-valid-until=no] https://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT}/ trixie main" \
    "deb [check-valid-until=no] https://snapshot.debian.org/archive/debian-security/${DEBIAN_SNAPSHOT}/ trixie-security main" \
    > /etc/apt/sources.list && \
    rm -f /etc/apt/sources.list.d/*.sources && \
    apt-get update && \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        erofs-utils=1.8.6-1 \
        cryptsetup=2:2.7.5-2 && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY requirements.txt .
COPY pack.py .

ENV CACHE_DIR="/cache"
ENV OUTPUT_DIR="/output"

RUN pip install --no-cache-dir --require-hashes -r requirements.txt

ENTRYPOINT ["python3", "pack.py"]
