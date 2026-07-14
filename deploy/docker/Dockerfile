ARG VEQRI_VERSION=0.1.0-dev
ARG VEQRI_COMMIT=unknown
ARG VEQRI_BUILD_TIME=unknown
ARG VEQRI_RELEASE=false

FROM golang:1.26.5-alpine AS build
ARG VEQRI_VERSION
ARG VEQRI_COMMIT
ARG VEQRI_BUILD_TIME
ARG VEQRI_RELEASE
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN case "$VEQRI_RELEASE" in \
      true) release_flag=--release ;; \
      false) release_flag= ;; \
      *) echo "VEQRI_RELEASE must be true or false" >&2; exit 1 ;; \
    esac \
    && VEQRI_VERSION="$VEQRI_VERSION" VEQRI_COMMIT="$VEQRI_COMMIT" VEQRI_BUILD_TIME="$VEQRI_BUILD_TIME" \
      CGO_ENABLED=0 go run ./cmd/veqri-build $release_flag --strip binaries

FROM alpine:3.23
LABEL org.opencontainers.image.title="Veqri Core"
RUN addgroup -S veqri && adduser -S -G veqri -h /var/lib/veqri veqri \
    && install -d -m 0700 -o veqri -g veqri /var/lib/veqri/data \
    && install -d -m 0700 -o veqri -g veqri /var/lib/veqri/workspaces/default
COPY --from=build /src/build/bin/veqri-core /usr/local/bin/veqri-core
COPY --from=build /src/build/bin/veqri /usr/local/bin/veqri
COPY --from=build /src/build/release/buildinfo.json /usr/share/veqri/buildinfo.json
USER veqri
WORKDIR /var/lib/veqri/workspaces/default
ENV VEQRI_ADDR=127.0.0.1:7342 \
    VEQRI_DATA_DIR=/var/lib/veqri/data \
    VEQRI_DATABASE=/var/lib/veqri/data/veqri.db \
    VEQRI_WORKSPACES=/var/lib/veqri/workspaces/default
VOLUME ["/var/lib/veqri"]
ENTRYPOINT ["/usr/local/bin/veqri-core"]
