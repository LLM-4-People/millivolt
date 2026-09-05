# The tag tracks go.mod; a source check rejects toolchain drift.
FROM --platform=$BUILDPLATFORM golang:1.27.1-bookworm@sha256:648f440f42a0958804efb24df176f806f9d353b41f1c0627f666428e40310f6b AS build
WORKDIR /src
ENV GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN go mod download
COPY VERSION version.go LICENSE THIRD_PARTY_NOTICES.md ./
COPY cmd/proxy ./cmd/proxy
COPY internal ./internal
COPY scripts/licenses ./scripts/licenses
ARG TARGETOS
ARG TARGETARCH
ARG REVISION
# The collector runs on BUILDPLATFORM and queries the target graph itself.
RUN mkdir -p /out/data /out/config && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -mod=readonly -trimpath -buildvcs=false \
      -ldflags="-s -X $(go list -m).revisionOverride=$REVISION" -o /out/millivolt ./cmd/proxy && \
    go run -mod=readonly ./scripts/licenses -target "$TARGETOS/$TARGETARCH" -out /out/licenses

FROM gcr.io/distroless/static-debian13:nonroot@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7
COPY --from=build /out/millivolt /millivolt
COPY --from=build /out/licenses /usr/share/licenses/millivolt
COPY --from=build --chown=65532:65532 /out/data/ /data/
COPY --from=build --chown=65532:65532 /out/config/ /config/
USER 65532:65532
WORKDIR /data
EXPOSE 8080
STOPSIGNAL SIGTERM
ENTRYPOINT ["/millivolt"]
CMD ["-config", "/config/proxy.yaml", "-db-path", "/data/proxy.db"]
