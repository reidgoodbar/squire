FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

ARG TARGETOS
ARG TARGETARCH
ARG SQUIRE_VERSION=dev

WORKDIR /src

COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
	go build -ldflags "-X squire/internal/buildinfo.Version=v${SQUIRE_VERSION}" -o /out/squire ./cmd/squire

FROM alpine:3.20

RUN apk add --no-cache ca-certificates && \
	adduser -D -h /home/squire squire

COPY --from=build /out/squire /usr/local/bin/squire
COPY registry/entrypoint.sh /usr/local/bin/entrypoint.sh

RUN chmod +x /usr/local/bin/squire /usr/local/bin/entrypoint.sh

LABEL org.opencontainers.image.source="https://github.com/reidgoodbar/squire"
LABEL org.opencontainers.image.description="Squire MCP stdio server wrapper"
LABEL org.opencontainers.image.licenses="MIT"
LABEL io.modelcontextprotocol.server.name="io.github.reidgoodbar/squire"

USER squire
ENV HOME=/home/squire

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
