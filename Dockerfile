ARG GO_VERSION=latest
ARG LIBDAVE_VERSION=v1.1.1

FROM golang:${GO_VERSION} as builder

ARG LIBDAVE_VERSION

WORKDIR /src
COPY . .

WORKDIR /src/cmd/bot

RUN apt-get update \
	&& apt-get install -y --no-install-recommends clang git ca-certificates bash pkg-config build-essential libusb-1.0-0-dev unzip cmake nasm zip \
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

FROM gcr.io/distroless/base as runtime

COPY --from=builder /bin/runner /
COPY --from=builder /runtime-libs/ /usr/local/lib/

ENV LD_LIBRARY_PATH=/usr/local/lib

CMD ["/runner"]
