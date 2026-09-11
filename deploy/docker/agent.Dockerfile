FROM --platform=$BUILDPLATFORM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/tetherd-agent ./cmd/tetherd-agent

# distroless "static" runs as root by default, which SYS_PTRACE needs (spec §5.3).
FROM gcr.io/distroless/static-debian12:latest
COPY --from=build /out/tetherd-agent /tetherd-agent
ENTRYPOINT ["/tetherd-agent"]
