FROM --platform=$BUILDPLATFORM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/sampleapp ./examples/sampleapp

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/sampleapp /sampleapp
EXPOSE 8081
HEALTHCHECK --interval=2s --timeout=2s --retries=15 CMD ["/sampleapp", "-check"]
ENTRYPOINT ["/sampleapp"]
