FROM ubuntu:24.04@sha256:fe81560990fae86dc4743202b2cb4d3df98d0bcf949173cecef15ce1127ef852
RUN apt-get update -qq && apt-get install -y --no-install-recommends gcc libc6-dev libipsec-mb-dev=1.5-1build1 && rm -rf /var/lib/apt/lists/*
COPY sign.c /src/sign.c
RUN mkdir /build && gcc -O3 -fPIC -shared -Wall -Wextra -Wl,--exclude-libs,ALL -o /build/libisureal-crypto.so /src/sign.c /usr/lib/x86_64-linux-gnu/libIPSec_MB.a && strip --strip-unneeded /build/libisureal-crypto.so
