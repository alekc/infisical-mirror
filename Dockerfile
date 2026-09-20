# goreleaser cross-compiles the binary and passes it in, so there is no build
# stage here: the context already holds a binary for the target platform. The
# base is distroless/static rather than scratch because the tool talks HTTPS to
# both instances and needs the CA bundle; on scratch every request would fail
# certificate verification, which reads as an outage rather than a packaging
# mistake.
FROM gcr.io/distroless/static:nonroot

COPY infisical-mirror /usr/local/bin/infisical-mirror

# The state file is the record of what this tool has already reconciled, so it
# has to outlive the container. Mount a volume here; without one, every start
# is a first run and every key looks new on both sides.
VOLUME ["/var/lib/infisical-mirror"]

# --daemon serves /metrics and /healthz on this port. A one-shot run never
# listens, so exposing it is a no-op there.
EXPOSE 9090

USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/infisical-mirror"]
CMD ["plan", "--config", "/etc/infisical-mirror/config.yaml"]
