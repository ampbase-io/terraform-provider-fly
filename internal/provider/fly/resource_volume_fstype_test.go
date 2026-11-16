package fly

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
)

// TestVolumeFstypeValidation pins what the fstype enum may and may not
// reject. The validator runs on config alone, so a volume that leaves
// fstype unset keeps planning whatever its state holds: existing volumes
// are only affected if their config names a type Fly does not offer.
func TestVolumeFstypeValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		config  string
		wantErr bool
	}{
		{"unset", `{"app":"a","name":"data","region":"iad","size_gb":1}`, false},
		{"ext4", `{"app":"a","name":"data","region":"iad","size_gb":1,"fstype":"ext4"}`, false},
		{"raw", `{"app":"a","name":"data","region":"iad","size_gb":1,"fstype":"raw"}`, false},
		{"unknown type", `{"app":"a","name":"data","region":"iad","size_gb":1,"fstype":"xfs"}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			diags := validateVolumeConfig(t, tt.config)
			var errs []*tfprotov6.Diagnostic
			for _, d := range diags {
				if d.Severity == tfprotov6.DiagnosticSeverityError {
					errs = append(errs, d)
				}
			}
			if got := len(errs) > 0; got != tt.wantErr {
				for _, e := range errs {
					t.Logf("%s: %s", e.Summary, e.Detail)
				}
				t.Fatalf("got %d errors, wantErr %v", len(errs), tt.wantErr)
			}
		})
	}
}

func validateVolumeConfig(t *testing.T, configJSON string) []*tfprotov6.Diagnostic {
	t.Helper()
	ctx := context.Background()

	var sresp resource.SchemaResponse
	NewVolumeResource().Schema(ctx, resource.SchemaRequest{}, &sresp)
	if sresp.Diagnostics.HasError() {
		t.Fatalf("schema: %v", sresp.Diagnostics)
	}
	typ := sresp.Schema.Type().TerraformType(ctx)
	val, err := (tfprotov6.RawState{JSON: []byte(configJSON)}).Unmarshal(typ)
	if err != nil {
		t.Fatalf("config does not decode against the schema: %v", err)
	}
	dv, err := tfprotov6.NewDynamicValue(typ, val)
	if err != nil {
		t.Fatalf("encoding config: %v", err)
	}

	srv := providerserver.NewProtocol6(New("test")())()
	resp, err := srv.ValidateResourceConfig(ctx, &tfprotov6.ValidateResourceConfigRequest{
		TypeName: "fly_volume",
		Config:   &dv,
	})
	if err != nil {
		t.Fatalf("ValidateResourceConfig: %v", err)
	}
	return resp.Diagnostics
}
