# Static armv7 hostapd for the Kindle Voyage.
#
# Built as armv7 under QEMU emulation rather than with a cross-toolchain: the
# Kindle is armv7l (i.MX6SL, Cortex-A9, VFPv3+NEON) and hostapd is small enough
# that emulated compilation is cheap. Linking fully static against musl means the
# result does not care what glibc the Kindle ships.
FROM --platform=linux/arm/v7 alpine:3.19

RUN apk add --no-cache build-base linux-headers curl pkgconf bison flex

WORKDIR /src

# --- libnl (static) -------------------------------------------------------
# hostapd's nl80211 driver needs libnl. Alpine does not reliably ship static
# archives, so build it here with shared libs disabled.
ARG LIBNL_VER=3.7.0
RUN curl -fsSL -o libnl.tar.gz \
      "https://github.com/thom311/libnl/releases/download/libnl3_7_0/libnl-${LIBNL_VER}.tar.gz" \
 && tar xf libnl.tar.gz \
 && cd "libnl-${LIBNL_VER}" \
# Install into /usr, not /opt: hostapd's Makefile hardcodes `-lnl-3 -lnl-genl-3`
# and `-I/usr/include/libnl3` for CONFIG_LIBNL32 and never consults pkg-config,
# so a custom prefix is simply invisible to the link step. --disable-shared means
# only the static archives exist, which is what we want to link against anyway.
 && ./configure --prefix=/usr --libdir=/usr/lib --enable-static --disable-shared \
      --disable-cli --disable-pthreads \
 && make -j"$(nproc)" \
 && make install

# --- hostapd (static) -----------------------------------------------------
ARG HOSTAPD_VER=2.11
RUN curl -fsSL -o hostapd.tar.gz "https://w1.fi/releases/hostapd-${HOSTAPD_VER}.tar.gz" \
 && tar xf hostapd.tar.gz

COPY hostapd.defconfig /src/hostapd.defconfig

# CFLAGS/LDFLAGS must come from the ENVIRONMENT, not the make command line.
# hostapd's Makefile does `CFLAGS += -I../src -I../src/utils`; a command-line
# CFLAGS= would override that outright and every file fails on utils/includes.h.
# From the environment, the Makefile's += appends to ours instead.
RUN cd "hostapd-${HOSTAPD_VER}/hostapd" \
 && cp /src/hostapd.defconfig .config \
 && export CFLAGS="-Os" \
 && export LDFLAGS="-static" \
 && make -j"$(nproc)" \
 && strip hostapd hostapd_cli \
 && mkdir -p /out \
 && cp hostapd hostapd_cli /out/

# Fail the build loudly if we did not actually get a static ARM binary.
RUN file /out/hostapd || true; \
    readelf -h /out/hostapd | grep -E 'Machine|Type'; \
    if readelf -d /out/hostapd 2>/dev/null | grep -q NEEDED; then \
        echo "ERROR: hostapd is dynamically linked"; exit 1; \
    fi; \
    echo "OK: static ARM hostapd"; ls -l /out
