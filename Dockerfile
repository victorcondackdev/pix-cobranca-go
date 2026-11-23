# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOWORK=off
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
# go.sum is produced by `go mod tidy` (CI runs it before building the image).
RUN test -f go.sum || go mod tidy
ARG VERSION=dev
RUN go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/mockpsp ./cmd/mockpsp

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mockpsp /mockpsp
USER nonroot:nonroot
# The development PKI is generated here on first start. Mount a volume to
# keep it across restarts and to hand the client certificate to the SDK.
ENV MOCKPSP_ADDR=:8443 \
    MOCKPSP_CERTS=/home/nonroot/certs
EXPOSE 8443
HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
  CMD ["/mockpsp", "healthcheck", "-url", "https://localhost:8443/healthz"]
ENTRYPOINT ["/mockpsp"]
CMD ["serve"]
