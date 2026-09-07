# The Vault image: upstream Vault plus the plugins this cluster registers
# and the scripts its containers run. Built by ../main.tf through Fly's
# remote builder; a change to anything COPYd here produces a new image
# label and rolls the machines onto it.
FROM hashicorp/vault:1.20
COPY plugins/ /vault/plugins/
COPY scripts/register-vault-plugins scripts/vault-ready scripts/fetch-oidc-token /usr/local/bin/
