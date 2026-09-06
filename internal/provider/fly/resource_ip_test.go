package fly

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// TestIPResource_SchemaAcceptsPublicTypes pins the RFC-022 extension:
// the type validator must accept all four IP types (private_v6 +
// shared_v4 + public_v4 + public_v6) so the env module can stamp out
// both Flycast and public app surface in one apply.
func TestIPResource_SchemaAcceptsPublicTypes(t *testing.T) {
	r := NewIPResource()
	var resp resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &resp)

	typeAttr, ok := resp.Schema.Attributes["type"].(schema.StringAttribute)
	if !ok {
		t.Fatal("type attribute missing or wrong type")
	}
	if len(typeAttr.Validators) == 0 {
		t.Fatal("type attribute has no validators")
	}

	// Drive the validator at each value the env module needs to pass
	// at plan time.
	wantValid := []string{"private_v6", "shared_v4", "public_v4", "public_v6"}
	for _, val := range wantValid {
		t.Run(val, func(t *testing.T) {
			req := validator.StringRequest{
				ConfigValue: types.StringValue(val),
			}
			var got validator.StringResponse
			for _, v := range typeAttr.Validators {
				v.ValidateString(context.Background(), req, &got)
			}
			if got.Diagnostics.HasError() {
				t.Errorf("validators rejected %q: %v", val, got.Diagnostics)
			}
		})
	}

	t.Run("bogus type rejected", func(t *testing.T) {
		req := validator.StringRequest{
			ConfigValue: types.StringValue("ipv7"),
		}
		var got validator.StringResponse
		for _, v := range typeAttr.Validators {
			v.ValidateString(context.Background(), req, &got)
		}
		if !got.Diagnostics.HasError() {
			t.Errorf("validators accepted bogus type")
		}
	})
}

// TestCertResource_Schema pins the fly_cert contract: hostname and app
// are required+replace, dns_validation fields (cname / acme_challenge_*
// / ownership_*) are computed so a downstream DNS module can subscribe
// to them in the same apply.
func TestCertResource_Schema(t *testing.T) {
	r := NewCertResource()
	var resp resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &resp)

	required := []string{"app", "hostname"}
	for _, name := range required {
		attr, ok := resp.Schema.Attributes[name].(schema.StringAttribute)
		if !ok {
			t.Errorf("%s missing or wrong type", name)
			continue
		}
		if !attr.Required {
			t.Errorf("%s should be required", name)
		}
		if len(attr.PlanModifiers) == 0 {
			t.Errorf("%s should be RequiresReplace", name)
		}
	}

	computed := []string{
		"configured", "status", "dns_provider",
		"cname", "a_records", "aaaa_records",
		"acme_challenge_name", "acme_challenge_target",
		"ownership_name", "ownership_app_value",
		"dns_configured", "http_configured", "alpn_configured",
	}
	for _, name := range computed {
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
		case schema.ListAttribute:
			if !a.Computed {
				t.Errorf("%s should be computed", name)
			}
		}
	}
}

// TestValidateIPTypeFieldCompatibility pins the plan-time check that
// catches the misconfiguration the docs cross-check flagged: network /
// org_slug are silently ignored by Fly's API for non-Flycast types.
// Without this check the operator would see a public IP allocated with
// the network they specified mysteriously missing.
func TestValidateIPTypeFieldCompatibility(t *testing.T) {
	cases := []struct {
		name           string
		ipType         string
		networkSet     bool
		orgSlugSet     bool
		wantNetworkErr bool
		wantOrgErr     bool
	}{
		{"public_v4 with network", "public_v4", true, false, true, false},
		{"public_v4 with org_slug", "public_v4", false, true, false, true},
		{"shared_v4 with both", "shared_v4", true, true, true, true},
		{"public_v6 with network", "public_v6", true, false, true, false},
		{"private_v6 with both is fine", "private_v6", true, true, false, false},
		{"public type with neither is fine", "public_v4", false, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diags := validateIPTypeFieldCompatibility(tc.ipType, tc.networkSet, tc.orgSlugSet)
			gotNetworkErr, gotOrgErr := false, false
			for _, d := range diags {
				switch d.Summary() {
				case "network Only Applies to Flycast":
					gotNetworkErr = true
				case "org_slug Only Applies to Flycast":
					gotOrgErr = true
				}
			}
			if gotNetworkErr != tc.wantNetworkErr {
				t.Errorf("network diag = %v, want %v (diags: %v)", gotNetworkErr, tc.wantNetworkErr, diags)
			}
			if gotOrgErr != tc.wantOrgErr {
				t.Errorf("org_slug diag = %v, want %v (diags: %v)", gotOrgErr, tc.wantOrgErr, diags)
			}
		})
	}
}

// TestInferIPType pins the import-time type heuristic. v4 → public_v4,
// fdaa: v6 → private_v6 (Flycast), other v6 → public_v6. Operator can
// rewrite to shared_v4 after import if needed — the API doesn't
// distinguish in the listing response.
func TestInferIPType(t *testing.T) {
	cases := map[string]string{
		"66.241.124.1":   "public_v4",
		"2a09:8280:1::1": "public_v6",
		"fdaa:0:3::1":    "private_v6",
	}
	for addr, want := range cases {
		t.Run(addr, func(t *testing.T) {
			if got := inferIPType(addr); got != want {
				t.Errorf("inferIPType(%q) = %q, want %q", addr, got, want)
			}
		})
	}
}

// TestCertResource_ImportStateRejectsBadID pins the cert ImportState
// strings.Cut error path — IDs without '/' or with empty halves get a
// clear diagnostic rather than confusing partial state.
func TestCertResource_ImportStateRejectsBadID(t *testing.T) {
	r := NewCertResource().(*certResource)
	bad := []string{"no-slash-here", "", "/", "app/", "/host.example.com"}
	for _, id := range bad {
		t.Run(id, func(t *testing.T) {
			resp := &resource.ImportStateResponse{State: tfsdk.State{Schema: schema.Schema{}}}
			r.ImportState(context.Background(), resource.ImportStateRequest{ID: id}, resp)
			if !resp.Diagnostics.HasError() {
				t.Errorf("import ID %q should have errored", id)
			}
		})
	}
}
