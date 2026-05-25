ARG GO_VERSION=latest
ARG LIBDAVE_VERSION=v1.1.1

FROM golang:${GO_VERSION} AS builder

ARG LIBDAVE_VERSION

WORKDIR /src
COPY . .

WORKDIR /src/cmd/bot

RUN apt-get update \
	&& apt-get install -y --no-install-recommends clang git ca-certificates bash pkg-config build-essential libusb-1.0-0-dev unzip cmake nasm zip libopus-dev libopusfile-dev \
	&& git clone https://github.com/disgoorg/godave /tmp/godave \
	&& chmod +x /tmp/godave/scripts/libdave_install.sh \
	&& /bin/bash /tmp/godave/scripts/libdave_install.sh $LIBDAVE_VERSION

ENV PKG_CONFIG_PATH="/root/.local/lib/pkgconfig"

RUN CGO_ENABLED=1 go build \
    -o /bin/runner

# Collect all shared library dependencies of the binary
RUN mkdir -p /runtime-libs && \
    ldd /bin/runner \
        | grep "=> /" \
        | awk '{print $3}' \
        | xargs -I{} cp --dereference {} /runtime-libs/

FROM gcr.io/distroless/base AS runtime

LABEL org.opencontainers.image.title="go-discord-caller" \
      org.opencontainers.image.description="Go Discord bot that captures voice audio and relays it live to every bound speaker bot — across one or multiple Discord servers simultaneously." \
      org.opencontainers.image.url="https://github.com/sealbro/go-discord-caller" \
      org.opencontainers.image.source="https://github.com/sealbro/go-discord-caller" \
      org.opencontainers.image.authors="sealbro <8067559+sealbro@users.noreply.github.com>" \
      org.opencontainers.image.licenses="Apache-2.0"

COPY --from=builder /bin/runner /
COPY --from=builder /runtime-libs/ /usr/local/lib/

ENV LD_LIBRARY_PATH=/usr/local/lib

CMD ["/runner"]
