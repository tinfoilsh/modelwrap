FROM debian:bookworm-slim

RUN apt update
RUN DEBIAN_FRONTEND=noninteractive apt install -y \
    erofs-utils=1.5-1 cryptsetup=2:2.6.1-4~deb12u2 python3 python3-pip python3-venv curl

RUN ARCH=$(dpkg --print-architecture) \
    && curl -sSL https://github.com/modelpack/modctl/releases/download/v0.1.0-alpha.0/modctl-0.1.0-alpha.0-linux-${ARCH}.deb -o modctl.deb \
    && dpkg -i modctl.deb \
    && rm modctl.deb
    
WORKDIR /app
COPY requirements.txt .
COPY pack.py .

RUN python3 -m venv /opt/venv
ENV PATH="/opt/venv/bin:$PATH"

ENV CACHE_DIR="/cache"
ENV OUTPUT_DIR="/output"

RUN pip install --no-cache-dir -r requirements.txt

CMD ["python3", "pack.py"]
