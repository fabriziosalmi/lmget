# lmget in a container: the release binary on distroless static, which holds
# CA certificates, a nonroot user and nothing else. release.yml builds it from
# the archives it has just checked; dist/linux_<arch>/lmget is the binary of
# each platform. COPY only: building it runs no code.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
ARG TARGETARCH
LABEL org.opencontainers.image.title="lmget" \
      org.opencontainers.image.description="Get open models from their sources, verified" \
      org.opencontainers.image.source="https://github.com/fabriziosalmi/lmget" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY dist/linux_${TARGETARCH}/lmget /usr/local/bin/lmget
ENV LMGET_HOME=/home/nonroot/.lmget \
    HF_HUB_CACHE=/home/nonroot/.cache/huggingface/hub
WORKDIR /home/nonroot
ENTRYPOINT ["/usr/local/bin/lmget"]
CMD ["--help"]
