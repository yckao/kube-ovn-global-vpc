# The Linux/amd64 hook helper is built and tested in the release binary matrix.
ARG CONTROLLER_BASE
FROM ${CONTROLLER_BASE}
ARG VERSION
ARG SOURCE_COMMIT
LABEL org.opencontainers.image.source="https://github.com/yckao/kube-ovn-global-vpc" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${SOURCE_COMMIT}"
COPY --chown=65532:65532 --chmod=0755 vpcctl /vpcctl
COPY LICENSE NOTICE THIRD_PARTY_NOTICES.md /usr/share/licenses/global-vpc/
USER 65532:65532
ENTRYPOINT ["/vpcctl"]
