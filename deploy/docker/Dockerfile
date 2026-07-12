FROM golang:1.26.5-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/veqri-core ./cmd/veqri-core \
    && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/veqri ./cmd/veqri-cli

FROM alpine:3.23
RUN addgroup -S veqri && adduser -S -G veqri -h /var/lib/veqri veqri \
    && install -d -m 0700 -o veqri -g veqri /var/lib/veqri/data \
    && install -d -m 0700 -o veqri -g veqri /var/lib/veqri/workspaces/default
COPY --from=build /out/veqri-core /usr/local/bin/veqri-core
COPY --from=build /out/veqri /usr/local/bin/veqri
USER veqri
WORKDIR /var/lib/veqri/workspaces/default
ENV VEQRI_ADDR=127.0.0.1:7342 \
    VEQRI_DATA_DIR=/var/lib/veqri/data \
    VEQRI_DATABASE=/var/lib/veqri/data/veqri.db \
    VEQRI_WORKSPACES=/var/lib/veqri/workspaces/default
VOLUME ["/var/lib/veqri"]
ENTRYPOINT ["/usr/local/bin/veqri-core"]
