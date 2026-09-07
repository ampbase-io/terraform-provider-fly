# Build-and-push through Fly's remote builder, inside the apply that needs
# the image. Owns the chicken-and-egg between an empty app and the image
# its machines start from:
#
#   1. The caller's fly_app creates the app, and with it the registry
#      namespace registry.fly.io/<app>.
#   2. terraform_data.push here builds and pushes to that namespace; the
#      dependency flows through var.app, so pass fly_app.<x>.name rather
#      than a literal.
#   3. The caller's fly_machine takes the `ref` output, which depends on
#      the push, so machines cannot race ahead of it.
#
# Wraps flyctl rather than the API because flyctl is Fly's paved path for
# builds and brings the free remote builder, so no Docker daemon is needed
# where tofu runs. The requirements are flyctl on PATH and FLY_API_TOKEN.
#
# var.context is the build context and must contain var.config and the
# Dockerfile that config names: flyctl needs a config to anchor a build
# for an app with no machines. The config's own `app` is a placeholder;
# the real app is passed with --app, so one config builds for every
# environment. A Dockerfile that COPYs from outside its directory points
# context at the wider root and names a config there.

terraform {
  required_version = ">= 1.9"
}

locals {
  # The image label is 12 hex characters of a sha256 over the trigger
  # files' contents and the build args, one `<path>:<sha256>` line per
  # file so a changed label is diagnosable to the file that moved. Any
  # change rebuilds and pushes; nothing changed is a no-op, and machines
  # keep the image they have. Build args are in the hash because a caller
  # that changes only an arg would otherwise keep the label and never push.
  label = substr(
    sha256(join("\n", concat(
      [for f in var.trigger_files : "${f}:${filesha256(f)}"],
      [for k in sort(keys(var.build_args)) : "arg:${k}=${var.build_args[k]}"],
    ))),
    0, 12,
  )

  ref = "registry.fly.io/${var.app}:${local.label}"

  # Absolute, so flyctl's working directory does not depend on where tofu
  # was invoked from (`tofu -chdir` shifts the implicit one).
  context_abs = abspath(var.context)

  # Values are passed verbatim; tofu does not shell-escape, so build args
  # must not carry whitespace or quotes.
  build_args_flags = join(" ", [
    for k, v in var.build_args : "--build-arg ${k}=${v}"
  ])
}

# Replaced, and so re-run, whenever the label changes. A failed build
# propagates its exit code and halts the apply before any fly_machine
# tries to pull an image that does not exist; re-applying retries at the
# same label.
#
# An image deleted out of band leaves the label unchanged, so this does
# not re-run and the next machine create fails to pull. Recovery is
# `tofu taint module.<name>.terraform_data.push`: the push fires again at
# the same label.
resource "terraform_data" "push" {
  triggers_replace = local.label

  provisioner "local-exec" {
    command     = "flyctl deploy --config ${var.config} --app ${var.app} --image-label ${local.label} --build-only --push ${var.build_mode} ${local.build_args_flags} ${var.extra_flyctl_args}"
    working_dir = local.context_abs
    interpreter = ["bash", "-c"]
  }
}
