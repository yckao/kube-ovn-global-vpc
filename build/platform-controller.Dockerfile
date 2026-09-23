# Build with repository root as context; the companion ignore file is an allowlist.
FROM golang:1.26.5 AS build
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api/ ./api/
COPY internal/ ./internal/
COPY cmd/platform-vpc-controller/ ./cmd/platform-vpc-controller/
COPY integration/kube-ovn/destinationroute/ ./integration/kube-ovn/destinationroute/
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -o /out/platform-vpc-controller ./cmd/platform-vpc-controller
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/platform-vpc-controller /usr/local/bin/platform-vpc-controller
COPY gateway/gateway.py gateway/overlay.py gateway/evpn.py gateway/managed.py /opt/global-vpc/gateway/
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/platform-vpc-controller"]
