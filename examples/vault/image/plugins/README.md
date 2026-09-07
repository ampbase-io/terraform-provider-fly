Plugin binaries for the image go here; the Dockerfile copies the directory
into /vault/plugins and the registrar registers each one by its sha256.
Every file here is part of the image label, so adding or replacing one
rebuilds and rolls the cluster.
