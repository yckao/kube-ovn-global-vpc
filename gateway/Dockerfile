ARG GATEWAY_BASE=alpine:3.21
FROM ${GATEWAY_BASE}
ARG VERSION=development
ARG SOURCE_COMMIT=unknown
LABEL org.opencontainers.image.source="https://github.com/yckao/kube-ovn-global-vpc" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${SOURCE_COMMIT}"
RUN apk add --no-cache frr wireguard-tools iproute2 util-linux python3 tcpdump iputils nftables iperf3
ENTRYPOINT ["python3", "/app/gateway.py"]
