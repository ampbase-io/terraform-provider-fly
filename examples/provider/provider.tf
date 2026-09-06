provider "fly" {
  org_slug = "my-org"
  # api_token defaults to FLY_API_TOKEN; use an org token from
  # `fly tokens create org my-org`.
}
