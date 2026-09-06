// Command terraform-provider-fly is a Terraform/OpenTofu provider for the
// Fly.io Machines API. See internal/provider/fly for the full list of
// resources; the current set is fly_app, fly_machine, fly_ip, fly_secret,
// fly_volume, fly_cert, and fly_cert_validation.
//
// Run with -debug to start in debug mode and attach a Terraform or OpenTofu
// CLI via the TF_REATTACH_PROVIDERS line it prints.
// Documentation under docs/ is generated from the schemas and examples/:
//
//go:generate go tool tfplugindocs generate --provider-name fly
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	fly "github.com/ampbase-io/terraform-provider-fly/internal/provider/fly"
)

var version = "dev"

func main() {
	debug := flag.Bool("debug", false, "run the provider in debug mode for attaching a CLI")
	flag.Parse()

	opts := providerserver.ServeOpts{
		Address: "registry.terraform.io/ampbase-io/fly",
		Debug:   *debug,
	}

	if err := providerserver.Serve(context.Background(), fly.New(version), opts); err != nil {
		log.Fatal(err)
	}
}
