package fly

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
)

// TestCertValidationResource_Schema pins the contract for the
// fly_cert_validation gate resource. cert_id is required+replace,
// validation_dependencies is the AWS-ACM-style opaque DAG anchor,
// configured is captured (not refreshed) by Read so a flapping cert
// doesn't show up as constant TF drift here.
func TestCertValidationResource_Schema(t *testing.T) {
	r := NewCertValidationResource()
	var resp resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &resp)

	cert, ok := resp.Schema.Attributes["cert_id"].(schema.StringAttribute)
	if !ok {
		t.Fatal("cert_id missing or wrong type")
	}
	if !cert.Required {
		t.Error("cert_id should be required")
	}
	if len(cert.PlanModifiers) == 0 {
		t.Error("cert_id should have RequiresReplace plan modifier")
	}

	deps, ok := resp.Schema.Attributes["validation_dependencies"].(schema.SetAttribute)
	if !ok {
		t.Fatal("validation_dependencies missing or wrong type")
	}
	if !deps.Optional {
		t.Error("validation_dependencies should be optional")
	}
	if len(deps.PlanModifiers) == 0 {
		t.Error("validation_dependencies should have RequiresReplace plan modifier")
	}

	timeout, ok := resp.Schema.Attributes["validation_timeout"].(schema.StringAttribute)
	if !ok {
		t.Fatal("validation_timeout missing or wrong type")
	}
	if !timeout.Optional || !timeout.Computed {
		t.Error("validation_timeout should be optional + computed (default applied on Create)")
	}

	for _, name := range []string{"configured", "status"} {
		attr, ok := resp.Schema.Attributes[name]
		if !ok {
			t.Errorf("missing computed attribute %q", name)
			continue
		}
		switch a := attr.(type) {
		case schema.StringAttribute:
			if !a.Computed {
				t.Errorf("%s should be computed", name)
			}
		case schema.BoolAttribute:
			if !a.Computed {
				t.Errorf("%s should be computed", name)
			}
		}
	}
}

// TestSplitCertID pins the cert_id parsing logic. The cert resource
// mints IDs as <app>/<hostname>; the validation resource needs to
// reverse that to call GetCertificate. Hostnames can contain dots and
// dashes but app names cannot contain "/" per Fly's naming rules.
func TestSplitCertID(t *testing.T) {
	cases := []struct {
		input    string
		wantApp  string
		wantHost string
		wantOk   bool
	}{
		{"ampbase-amp-staging/staging.ampbase.io", "ampbase-amp-staging", "staging.ampbase.io", true},
		{"app/*.example.com", "app", "*.example.com", true},
		{"app/sub.example.com/extra", "app", "sub.example.com/extra", true}, // hostnames don't contain / in practice but Cut splits only on the first separator
		{"no-slash", "", "", false},
		{"/empty-app", "", "", false},
		{"empty-host/", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			app, host, ok := splitCertID(tc.input)
			if ok != tc.wantOk {
				t.Errorf("ok = %v, want %v", ok, tc.wantOk)
			}
			if app != tc.wantApp {
				t.Errorf("app = %q, want %q", app, tc.wantApp)
			}
			if host != tc.wantHost {
				t.Errorf("host = %q, want %q", host, tc.wantHost)
			}
		})
	}
}
