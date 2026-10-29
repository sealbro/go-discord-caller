ARG GO_VERSION=latest
ARG LIBDAVE_VERSION=v1.1.1

FROM golang:${GO_VERSION} AS builder

ARG LIBDAVE_VERSION
ARG VERSION=dev

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
    -ldflags "-X github.com/sealbro/go-discord-caller/internal/buildinfo.Version=${VERSION} -X github.com/sealbro/go-discord-caller/internal/buildinfo.LibdaveVersion=${LIBDAVE_VERSION}" \
    -o /bin/runner

# Collect all shared library dependencies of the binary
RUN mkdir -p /runtime-libs && \
    ldd /bin/runner \
        | grep "=> /" \
        | awk '{print $3}' \
        | xargs -I{} cp --dereference {} /runtime-libs/

ARG GO_LICENSES_VERSION=v2.0.1

# Third-party licence texts for everything shipped in the runtime image
RUN go install github.com/google/go-licenses/v2@${GO_LICENSES_VERSION} \
	&& go-licenses save . --save_path=/licenses/go --ignore github.com/sealbro/go-discord-caller \
	&& cp /src/LICENSE /licenses/LICENSE

# install-libdave discards the licences shipped in libdave's release archive (its own plus the libraries it bundles)
RUN case "$(uname -m)" in x86_64) arch=X64 ;; aarch64) arch=ARM64 ;; *) echo "unsupported arch $(uname -m)" >&2; exit 1 ;; esac \
	&& curl -fsSL "https://github.com/discord/libdave/releases/download/${LIBDAVE_VERSION}%2Fcpp/libdave-Linux-${arch}-boringssl.zip" -o /tmp/libdave.zip \
	&& mkdir -p /licenses/libdave \
	&& unzip -j /tmp/libdave.zip 'licenses/*' -d /licenses/libdave \
	&& rm /tmp/libdave.zip

RUN mkdir -p /licenses/debian && \
    for lib in /runtime-libs/*; do \
        pkg=$(dpkg -S "*/$(basename "$lib")" 2>/dev/null | head -1 | cut -d: -f1); \
        if [ -n "$pkg" ] && [ -f "/usr/share/doc/$pkg/copyright" ]; then \
            cp "/usr/share/doc/$pkg/copyright" "/licenses/debian/$pkg"; \
        fi; \
    done

FROM builder AS sec

ARG GOSEC_VERSION=v2.29.0
# UPSTREAM(gosec v2.29.0): bundles golang.org/x/tools v0.49.0, which cannot read
# Go 1.27.2 export data, so every package fails to type-check and nothing is
# scanned. Build it against a newer x/tools until a gosec release ships one.
ARG XTOOLS_VERSION=v0.51.0

WORKDIR /tmp/gosec
RUN go mod init gosec-build \
	&& go get github.com/securego/gosec/v2/cmd/gosec@${GOSEC_VERSION} golang.org/x/tools@${XTOOLS_VERSION} \
	&& go build -o /go/bin/gosec github.com/securego/gosec/v2/cmd/gosec

WORKDIR /src
RUN gosec -tags=integration,stress ./...

FROM gcr.io/distroless/base AS runtime

LABEL org.opencontainers.image.title="go-discord-caller" \
      org.opencontainers.image.description="Go Discord bot that captures voice audio and relays it live to every bound speaker bot — across one or multiple Discord servers simultaneously." \
      org.opencontainers.image.url="https://github.com/sealbro/go-discord-caller" \
      org.opencontainers.image.source="https://github.com/sealbro/go-discord-caller" \
      org.opencontainers.image.authors="Alex S. <alexs-oss@pm.me>" \
      org.opencontainers.image.licenses="Apache-2.0"

COPY --from=builder /bin/runner /
COPY --from=builder /runtime-libs/ /usr/local/lib/
COPY --from=builder /licenses/ /licenses/

ENV LD_LIBRARY_PATH=/usr/local/lib

CMD ["/runner"]
