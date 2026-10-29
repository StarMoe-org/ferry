FROM python:3.13-alpine
ARG SOPS_VERSION=3.13.3
ARG SOPS_SHA256=e5bec3346a873ae91d871550f3e698c1aad962aff462a080e40f25fde17fef6b
RUN apk add --no-cache git tini \
 && pip install --no-cache-dir pyyaml==6.0.3 \
 && wget -qO /usr/local/bin/sops "https://github.com/getsops/sops/releases/download/v${SOPS_VERSION}/sops-v${SOPS_VERSION}.linux.amd64" \
 && echo "${SOPS_SHA256}  /usr/local/bin/sops" | sha256sum -c - \
 && chmod +x /usr/local/bin/sops && sops --version
COPY relay.py /app/relay.py
USER 65534
ENV HOME=/tmp
ENTRYPOINT ["/sbin/tini", "--"]
CMD ["python", "-u", "/app/relay.py"]
