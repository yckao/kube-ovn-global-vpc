# Build from the repository root:
# docker build -f build/site-controller.Dockerfile -t site-vpc-controller:dev .
# The accompanying Dockerfile-specific ignore file excludes private Lab state.
FROM golang:1.26.0 AS controller-build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api/ ./api/
COPY internal/ ./internal/
COPY cmd/site-vpc-controller/ ./cmd/site-vpc-controller/
RUN test "$TARGETOS" = linux && test -n "$TARGETARCH" \
    && CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
       go build -trimpath -o /out/site-vpc-controller ./cmd/site-vpc-controller

FROM alpine:3.21 AS kubectl-download
# Override to match the target API server's supported kubectl version.
ARG KUBECTL_VERSION=v1.36.2
ARG TARGETARCH
RUN apk add --no-cache ca-certificates curl
WORKDIR /out
RUN test -n "$TARGETARCH" \
    && curl --fail --silent --show-error --location --retry 3 \
       --output kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" \
    && curl --fail --silent --show-error --location --retry 3 \
       --output kubectl.sha256 "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl.sha256" \
    && printf '%s  kubectl\n' "$(cat kubectl.sha256)" | sha256sum -c - \
    && chmod 0755 kubectl

FROM alpine:3.21
RUN apk add --no-cache ca-certificates python3 \
    && addgroup -g 65532 controller \
    && adduser -D -H -u 65532 -G controller controller
COPY --from=controller-build /out/site-vpc-controller /usr/local/bin/site-vpc-controller
COPY --from=kubectl-download /out/kubectl /usr/local/bin/kubectl
WORKDIR /opt/global-vpc
COPY scripts/site_gateway.py scripts/transport_profiles.py ./scripts/
COPY gateway/gateway.py gateway/overlay.py gateway/evpn.py ./gateway/
# The deployment mounts a writable emptyDir at /tmp for kubectl's cache.
ENV HOME=/tmp \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/site-vpc-controller"]
