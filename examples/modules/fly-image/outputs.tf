output "ref" {
  description = "registry.fly.io/<app>:<label>, for fly_machine.image. Depends on the push, so a consumer cannot run ahead of it."
  value       = local.ref

  depends_on = [terraform_data.push]
}

output "label" {
  description = "The 12-character content label, for cross-referencing with the registry."
  value       = local.label
}
