# ClickHouse Keeper, layered over the upstream image. The cluster-specific
# keeper_config.xml is written per machine by Terraform (the file block in
# ../../main.tf); this image only adds an entrypoint shim that chowns the
# data directory, which upstream's entrypoint does not and Fly's volume
# mount requires.
#
# Pinned to the full patch version, and moved together with
# ../server/Dockerfile: the label the machines pull is a hash of this file,
# so a floating tag would let an unrelated rebuild pull a different patch
# under an unchanged label, and would not deliver patches on its own either
# since nothing rebuilds on a schedule.
FROM clickhouse/clickhouse-keeper:26.3.21.7

COPY docker-entrypoint-fly.sh /docker-entrypoint-fly.sh
RUN chmod +x /docker-entrypoint-fly.sh

ENTRYPOINT ["/docker-entrypoint-fly.sh"]
