FROM alpine:3.21
RUN apk add --no-cache frr wireguard-tools iproute2 util-linux python3 tcpdump iputils nftables iperf3
ENTRYPOINT ["python3", "/app/gateway.py"]
