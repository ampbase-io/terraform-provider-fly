variable "app" {
  description = "Fly app that owns the registry namespace; the image goes to registry.fly.io/<app>:<label>. Pass fly_app.<x>.name so the push waits for the app."
  type        = string
}

variable "context" {
  description = "Docker build context. flyctl runs here, so COPY paths in the Dockerfile resolve from it. Must contain var.config and the Dockerfile it names."
  type        = string
}

variable "config" {
  description = "flyctl app config, relative to var.context. Its `app` value is overridden by var.app."
  type        = string
  default     = "fly.toml"
}

variable "trigger_files" {
  description = "Files whose contents determine the label: the Dockerfile, the config, and everything the Dockerfile COPYs in. A change to any of them rebuilds."
  type        = list(string)
  validation {
    condition     = length(var.trigger_files) > 0
    error_message = "trigger_files must name at least the Dockerfile and the config; with none the label is constant and the image never rebuilds."
  }
}

variable "build_args" {
  description = "--build-arg pairs, passed verbatim."
  type        = map(string)
  default     = {}
}

variable "extra_flyctl_args" {
  description = "Extra flags for flyctl deploy (e.g. --build-target)."
  type        = string
  default     = ""
}

variable "build_mode" {
  description = <<-EOT
    Where the image is built: "--remote-only" (Fly's remote builder) or
    "--local-only" (the operator's Docker daemon). The remote builder has a
    fixed memory size that flyctl exposes no flag for, so an image whose
    compile peaks above it has to build locally.
  EOT
  type        = string
  default     = "--remote-only"
  validation {
    condition     = contains(["--remote-only", "--local-only"], var.build_mode)
    error_message = "build_mode must be --remote-only or --local-only."
  }
}
