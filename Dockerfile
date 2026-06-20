# clever-vpn-base: Minimal Debian with VPN server dependencies
FROM debian:12-slim

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
    wireguard-tools \
    nftables \
    curl \
    ca-certificates \
    systemd \
    systemd-sysv \
    && apt-get clean && \
    rm -rf /var/lib/apt/lists/*

# systemd needs these
RUN rm -f /etc/systemd/system/*.wants/*
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
