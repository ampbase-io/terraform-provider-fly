# ClickHouse server with the configuration that is the same on every
# machine baked in: the cold-tier storage policy (its endpoint and
# credentials come from the environment), the Prometheus endpoint the
# sidecar scrapes, and console logging so `fly logs` sees the server.
# Cluster topology (remote_servers.xml, macros.xml) differs per machine and
# is written by Terraform's file blocks instead.
#
# No entrypoint shim here, unlike ../keeper: upstream's server entrypoint
# already chowns /var/lib/clickhouse before dropping privileges.
#
# Same version as ../keeper/Dockerfile, pinned in full for the same reason.
FROM clickhouse/clickhouse-server:26.3.21.7

COPY storage.xml /etc/clickhouse-server/config.d/storage.xml
COPY prometheus.xml /etc/clickhouse-server/config.d/prometheus.xml
COPY logging.xml /etc/clickhouse-server/config.d/logging.xml
