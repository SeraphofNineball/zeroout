FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/zeroout ./cmd/zeroout

FROM scratch
COPY --from=build /out/zeroout /zeroout
ENV ZEROOUT_LISTEN=:8080 \
    ZEROOUT_TARGETS=/targets \
    ZEROOUT_DATA=/data
EXPOSE 8080
VOLUME /data
HEALTHCHECK --interval=30s --timeout=5s CMD ["/zeroout", "-healthcheck"]
# Runs as root on purpose: only root can write into ext4's reserved blocks
# ("fill completely" mode).
ENTRYPOINT ["/zeroout"]
