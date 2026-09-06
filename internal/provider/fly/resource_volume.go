package fly

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/ampbase-io/terraform-provider-fly/flyio"
	"github.com/ampbase-io/terraform-provider-fly/flyio/machines"
)

var (
	_ resource.Resource                = (*volumeResource)(nil)
	_ resource.ResourceWithConfigure   = (*volumeResource)(nil)
	_ resource.ResourceWithImportState = (*volumeResource)(nil)
	_ resource.ResourceWithModifyPlan  = (*volumeResource)(nil)
)

// volumeReadyTimeout caps how long Create blocks waiting for the volume
// to leave "creating" state. Tuned for the common case (sub-second for
// unforked volumes); snapshot-restored or forked volumes can take
// longer and may need a future schema-level override.
const volumeReadyTimeout = 5 * time.Minute

type volumeResource struct {
	pd *providerData
}

type volumeResourceModel struct {
	ID                     types.String        `tfsdk:"id"`
	App                    types.String        `tfsdk:"app"`
	Name                   types.String        `tfsdk:"name"`
	Region                 types.String        `tfsdk:"region"`
	SizeGB                 types.Int64         `tfsdk:"size_gb"`
	Encrypted              types.Bool          `tfsdk:"encrypted"`
	Fstype                 types.String        `tfsdk:"fstype"`
	AutoBackupEnabled      types.Bool          `tfsdk:"auto_backup_enabled"`
	SnapshotRetention      types.Int64         `tfsdk:"snapshot_retention"`
	RequireUniqueZone      types.Bool          `tfsdk:"require_unique_zone"`
	UniqueZoneAppWide      types.Bool          `tfsdk:"unique_zone_app_wide"`
	ReadoptOnHostMigration types.Bool          `tfsdk:"readopt_on_host_migration"`
	Compute                *volumeComputeModel `tfsdk:"compute"`

	// Computed.
	State             types.String `tfsdk:"state"`
	AttachedMachineID types.String `tfsdk:"attached_machine_id"`
	Zone              types.String `tfsdk:"zone"`
}

// volumeComputeModel mirrors the guestModel block on fly_machine — Fly
// uses these inputs at create time to place the volume on a host that
// can later schedule a machine of this shape (see schema description on
// the `compute` block).
type volumeComputeModel struct {
	CPUKind  types.String `tfsdk:"cpu_kind"`
	CPUs     types.Int64  `tfsdk:"cpus"`
	MemoryMB types.Int64  `tfsdk:"memory_mb"`
}

func NewVolumeResource() resource.Resource {
	return &volumeResource{}
}

func (r *volumeResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_volume"
}

func (r *volumeResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a Fly.io volume.\n\n" +
			"Volumes are **host-pinned**, not just region-pinned. A machine can only mount a volume that lives on the same physical host. Set the `compute` block to the matching machine's `guest` so Fly pre-places the volume on a host with capacity for that machine shape.\n\n" +
			"For HA topologies (a Raft quorum, database replicas), pick one of:\n" +
			"  * **Shared name + `require_unique_zone = true`** (the default): `count = N` with the same `name`, Fly spreads across distinct hardware zones. The documented Fly HA recipe.\n" +
			"  * **Distinct names + `unique_zone_app_wide = true`**: name each volume per-machine (e.g. `keeper_data_${count.index + 1}`) for operational clarity; app-wide zone uniqueness still gives HA placement.\n\n" +
			"Each volume attaches to exactly one machine at a time via a `fly_machine.mount` block. Volumes attached to a machine cannot be destroyed; `terraform destroy` order (machine before volume) is implicit via the mount.volume reference but a brief reconciliation lag may force a destroy re-run.\n\n" +
			"Size can be extended in-place; name/region/encrypted/fstype changes require replacement (and lose data). Shrinking is not supported by Fly; a planned size decrease is rejected at plan time.\n\n" +
			"Create blocks until the volume's state reaches `created` so the returned `id` is safe to feed directly into a `fly_machine.mount.volume` reference in the same apply.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Description: "Volume ID (e.g. \"vol_abc123\").",
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"app": schema.StringAttribute{
				Description: "Application name the volume belongs to.",
				Required:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"name": schema.StringAttribute{
				Description: "Volume name. Multiple machines in the same app can reference volumes with the same name when scaling horizontally (one volume per machine), but the API does not enforce uniqueness — keep names distinct per resource.",
				Required:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"region": schema.StringAttribute{
				Description: "Fly.io region (e.g. \"iad\"). Must match the machine the volume attaches to.",
				Required:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"size_gb": schema.Int64Attribute{
				Description: "Size in GiB. Increases trigger an in-place extend via the `/extend` endpoint. Decreases are rejected at plan time (Fly does not support shrinking volumes); see `ModifyPlan` for the enforcement.",
				Required:    true,
			},
			"encrypted": schema.BoolAttribute{
				Description: "Encrypt the volume at rest. Defaults to true. Cannot be changed after creation.",
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(true),
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.RequiresReplace(),
				},
			},
			"fstype": schema.StringAttribute{
				Description: "Filesystem type. Defaults to ext4. Cannot be changed after creation.",
				Optional:    true,
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"auto_backup_enabled": schema.BoolAttribute{
				Description: "Enable Fly's automatic daily snapshots. Defaults to true. Updateable in place.",
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(true),
			},
			"snapshot_retention": schema.Int64Attribute{
				Description: "How many days to keep auto-snapshots. Range 1-60, default 5 (set by Fly). Updateable in place; new retention applies to future snapshots only.",
				Optional:    true,
				Computed:    true,
			},
			"require_unique_zone": schema.BoolAttribute{
				Description: "When true (Fly's default), the volume is placed on hardware that doesn't already host another volume **with the same name**. Combine with a shared `name` and `count = N` to spread N replicas across distinct hardware zones (Fly's documented HA recipe). For distinct per-machine names, set `unique_zone_app_wide` instead. Set to false if you have more replicas than the region has hardware zones — Fly will collapse onto shared hosts.",
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(true),
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.RequiresReplace(),
				},
			},
			"unique_zone_app_wide": schema.BoolAttribute{
				Description: "When true, the zone-uniqueness constraint applies across **all volumes in the app**, regardless of name. Lets a cluster module give each volume a descriptive per-machine name (e.g. `keeper_data_1`, `keeper_data_2`, `keeper_data_3`) and still get HA placement on distinct hardware zones. Mutually intelligible with the same-name pattern: pick one. Not documented on fly.io/docs at time of writing; surfaced from the Machines API OpenAPI spec.",
				Optional:    true,
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.RequiresReplace(),
				},
			},
			"readopt_on_host_migration": schema.BoolAttribute{
				Description: "When true, recover automatically from a Fly host migration that forks this volume to a new ID. Fly may move the mounting machine to a new host and, because volumes are host-pinned, fork the volume to a **new** volume ID and re-mount it — leaving the volume this resource tracks detached (or destroyed). On refresh, if the tracked volume is gone or detached AND exactly one other volume in the app with the same `name` is currently attached to a machine, the resource re-anchors its `id` to that fork. This turns recovery from a Fly host replacement into a plain `terraform apply -refresh-only` with no state surgery. REQUIRES app-unique volume names (e.g. `clickhouse_data_${count.index + 1}`): the re-anchor keys on `name`, so leave this false when multiple live volumes share a name. A re-anchor emits a warning naming the old and new IDs so the orphaned volume can be destroyed. Defaults to false.\n\nNote: independently of this flag, a tracked volume that has been *deleted* (gone entirely, not merely detached) is adopted onto an unambiguous same-named attached volume rather than removed from state and recreated — dropping a live fork would be destructive. Shared-name / `count = N` topologies therefore carry a small adoption-ambiguity risk on a gone volume even with this flag left false; prefer app-unique per-slot names (`unique_zone_app_wide`) where that risk matters.",
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
			},
			"state": schema.StringAttribute{
				Description: "Volume state (e.g. \"ready\", \"creating\").",
				Computed:    true,
			},
			"attached_machine_id": schema.StringAttribute{
				Description: "ID of the machine the volume is currently attached to, or empty if detached.",
				Computed:    true,
			},
			"zone": schema.StringAttribute{
				Description: "Physical zone within the region (e.g. \"qaz1\"). Fly schedules volumes onto specific zones for availability.",
				Computed:    true,
			},
		},
		Blocks: map[string]schema.Block{
			"compute": schema.SingleNestedBlock{
				Description: "Guest config of the machine that will mount this volume. Fly uses this at create time to place the volume on a host with capacity for that machine shape. Match the `guest` block on the matching `fly_machine`.",
				Attributes: map[string]schema.Attribute{
					"cpu_kind": schema.StringAttribute{
						Description: "CPU kind: \"shared\" or \"performance\".",
						Optional:    true,
						PlanModifiers: []planmodifier.String{
							stringplanmodifier.RequiresReplace(),
						},
					},
					"cpus": schema.Int64Attribute{
						Description: "Number of CPUs.",
						Optional:    true,
					},
					"memory_mb": schema.Int64Attribute{
						Description: "Memory in megabytes.",
						Optional:    true,
					},
				},
			},
		},
	}
}

// ModifyPlan rejects a planned size_gb decrease at plan time so operators
// see the error from `tofu plan` instead of mid-apply. Fly does not
// support shrinking volumes; the only safe alternatives are forking
// from a snapshot into a smaller volume or destroying and recreating
// (which is data loss).
func (r *volumeResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		// Create (no prior state) or destroy (no plan) — nothing to compare.
		return
	}
	var state, plan volumeResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if plan.SizeGB.IsUnknown() || state.SizeGB.IsUnknown() {
		return
	}
	if plan.SizeGB.ValueInt64() < state.SizeGB.ValueInt64() {
		resp.Diagnostics.AddAttributeError(
			path.Root("size_gb"),
			"Cannot Shrink Volume",
			fmt.Sprintf("size_gb decreased from %d to %d; Fly volumes do not support shrinking. Fork from a snapshot into a smaller volume, or destroy and recreate (loses data).",
				state.SizeGB.ValueInt64(), plan.SizeGB.ValueInt64()),
		)
		return
	}

	// Host-migration re-anchor, plan-time. The Read-path re-anchor gates on
	// readopt_on_host_migration as stored in *prior state*, so it cannot fire
	// on the apply that first flips the flag from null/false to true — adding
	// the field to an already-managed volume, or a fresh import that doesn't
	// carry it. On that apply refresh leaves the stale pre-fork id in state,
	// the dependent fly_machine.mount.volume resolves to a volume Fly already
	// destroyed, and the machine update fails mid-apply with "volume does not
	// exist". Re-anchoring here keys on the *planned* flag, so the corrected
	// id lands in the plan before any dependent resolves the reference.
	//
	// The refreshed state is the cheap trigger: a volume the last refresh saw
	// still attached did not migrate, so the steady state skips the
	// ListVolumes probe entirely. Only a flag-on volume whose tracked id is
	// now detached (or gone) — the host-migration signature — warrants it.
	switch {
	case len(resp.RequiresReplace) > 0:
		return // forced replacement (e.g. a region change): state.ID is the
		// volume being destroyed, not one to re-anchor onto a live sibling.
	case r.pd == nil, plan.ReadoptOnHostMigration.IsUnknown(), !plan.ReadoptOnHostMigration.ValueBool():
		return
	case state.ID.IsNull(), state.ID.IsUnknown():
		return
	case !state.AttachedMachineID.IsNull() && state.AttachedMachineID.ValueString() != "":
		return // tracked volume was attached at refresh — no migration
	}

	trackedID := state.ID.ValueString()
	vols, err := r.pd.client.ListVolumes(ctx, state.App.ValueString())
	switch repl := pickReplacementVolume(vols, state.Name.ValueString(), trackedID); {
	case err != nil:
		resp.Diagnostics.AddError("Read Volume Error", err.Error())
	case repl == nil:
		// No unambiguous fork — leave the plan untouched.
	default:
		resp.Diagnostics.AddWarning(
			"Volume re-anchored after Fly host migration",
			fmt.Sprintf("Volume %q was tracked as %s but is now %s (attached to machine %s) — Fly forked it during a host migration. The plan re-anchors id to the fork so the dependent machine mount resolves. The old volume %s is orphaned and should be destroyed once confirmed unused.",
				state.Name.ValueString(), trackedID, repl.ID, repl.AttachedMachineID, trackedID),
		)
		plan.ID = types.StringValue(repl.ID)
		// The remaining computed identity is authoritative only after apply,
		// when Update re-reads the fork. Mark it unknown rather than copy
		// possibly stale values so the plan can't go inconsistent with the
		// applied result.
		plan.AttachedMachineID = types.StringUnknown()
		plan.Zone = types.StringUnknown()
		plan.State = types.StringUnknown()
		resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
	}
}

func (r *volumeResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	pd, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *providerData, got: %T", req.ProviderData))
		return
	}
	r.pd = pd
}

func (r *volumeResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan volumeResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client := r.pd.client
	appName := plan.App.ValueString()

	input := flyio.CreateVolumeInput{
		Name:   plan.Name.ValueString(),
		Region: plan.Region.ValueString(),
		SizeGB: int(plan.SizeGB.ValueInt64()),
	}
	if !plan.Encrypted.IsNull() && !plan.Encrypted.IsUnknown() {
		v := plan.Encrypted.ValueBool()
		input.Encrypted = &v
	}
	if !plan.Fstype.IsNull() && !plan.Fstype.IsUnknown() {
		input.Fstype = plan.Fstype.ValueString()
	}
	if !plan.AutoBackupEnabled.IsNull() && !plan.AutoBackupEnabled.IsUnknown() {
		v := plan.AutoBackupEnabled.ValueBool()
		input.AutoBackupEnabled = &v
	}
	if !plan.SnapshotRetention.IsNull() && !plan.SnapshotRetention.IsUnknown() {
		v := int(plan.SnapshotRetention.ValueInt64())
		input.SnapshotRetention = &v
	}
	if !plan.RequireUniqueZone.IsNull() && !plan.RequireUniqueZone.IsUnknown() {
		v := plan.RequireUniqueZone.ValueBool()
		input.RequireUniqueZone = &v
	}
	if !plan.UniqueZoneAppWide.IsNull() && !plan.UniqueZoneAppWide.IsUnknown() {
		v := plan.UniqueZoneAppWide.ValueBool()
		input.UniqueZoneAppWide = &v
	}
	if plan.Compute != nil {
		g := &machines.FlyMachineGuest{}
		if !plan.Compute.CPUKind.IsNull() && !plan.Compute.CPUKind.IsUnknown() {
			kind := plan.Compute.CPUKind.ValueString()
			g.CpuKind = &kind
		}
		if !plan.Compute.CPUs.IsNull() && !plan.Compute.CPUs.IsUnknown() {
			cpus := int(plan.Compute.CPUs.ValueInt64())
			g.Cpus = &cpus
		}
		if !plan.Compute.MemoryMB.IsNull() && !plan.Compute.MemoryMB.IsUnknown() {
			mem := int(plan.Compute.MemoryMB.ValueInt64())
			g.MemoryMb = &mem
		}
		input.Compute = g
	}

	info, err := client.CreateVolume(ctx, appName, input)
	if err != nil {
		resp.Diagnostics.AddError("Create Volume Error", err.Error())
		return
	}

	// Wait for the volume to leave "creating" before returning. Fly's
	// MachinesCreate is documented to block on volume readiness for any
	// mounts referenced in the request, but we wait here anyway so the
	// returned `id` is unambiguously safe to consume in the same apply
	// (cluster-bootstrap scenario where fly_machine.mount.volume references
	// fly_volume.X.id). Common path completes sub-second; forked or
	// snapshot-restored volumes can take longer.
	waitCtx, cancel := context.WithTimeout(ctx, volumeReadyTimeout)
	defer cancel()
	if err := client.WaitForVolumeReady(waitCtx, appName, info.ID); err != nil {
		resp.Diagnostics.AddError("Wait For Volume Ready Error", err.Error())
		return
	}

	// Re-read so the state we save reflects the post-wait values (state
	// field transitioned, zone may have been assigned during creation).
	info, err = client.GetVolume(ctx, appName, info.ID)
	if err != nil {
		resp.Diagnostics.AddError("Read Volume Error", err.Error())
		return
	}
	if info == nil {
		resp.Diagnostics.AddError("Read Volume Error", "volume disappeared after reaching ready state")
		return
	}

	setVolumeState(&plan, info)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *volumeResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state volumeResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client := r.pd.client
	app := state.App.ValueString()
	trackedID := state.ID.ValueString()

	info, err := client.GetVolume(ctx, app, trackedID)
	if err != nil {
		resp.Diagnostics.AddError("Read Volume Error", err.Error())
		return
	}

	// Fly host-migration recovery: when the tracked volume is gone or has
	// been detached (its machine was moved to a new host and the volume
	// forked to a new ID), re-anchor to the fork instead of removing the
	// resource or clinging to the orphan. See wantsReanchor and the schema
	// description; the adopted fork must still be unambiguous.
	if wantsReanchor(info, state.ReadoptOnHostMigration.ValueBool()) {
		vols, listErr := client.ListVolumes(ctx, app)
		if listErr != nil {
			resp.Diagnostics.AddError("Read Volume Error", listErr.Error())
			return
		}
		if repl := pickReplacementVolume(vols, state.Name.ValueString(), trackedID); repl != nil {
			resp.Diagnostics.AddWarning(
				"Volume re-anchored after Fly host migration",
				fmt.Sprintf("Volume %q was tracked as %s but is now %s (attached to machine %s) — Fly forked it during a host migration. State re-anchored to the fork. The old volume %s is orphaned and should be destroyed once confirmed unused.",
					state.Name.ValueString(), trackedID, repl.ID, repl.AttachedMachineID, trackedID),
			)
			setVolumeState(&state, repl)
			resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
			return
		}
	}

	if info == nil {
		// Volume was deleted out of band and no fork was adopted.
		resp.State.RemoveResource(ctx)
		return
	}

	setVolumeState(&state, info)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// wantsReanchor reports whether Read should look for a host-migration fork
// to adopt. A tracked volume that is gone always warrants the look: removing
// the resource here would force a destructive recreate, so an unambiguous
// live fork is strictly the safer adoption (and this is the case the flag
// cannot yet cover, on the apply that first enables it, when prior state
// still reads the flag as false). A detached-but-present orphan is the
// documented opt-in: re-anchor only when readopt is set. Either way the
// caller still declines on an ambiguous fork (pickReplacementVolume).
func wantsReanchor(info *flyio.VolumeInfo, readopt bool) bool {
	if info == nil {
		return true
	}
	return readopt && info.AttachedMachineID == ""
}

// pickReplacementVolume finds the single volume that supersedes trackedID
// after a Fly host migration: a same-named volume, other than the tracked
// one, currently attached to a machine. It returns nil when the match is
// ambiguous (zero or more than one candidate) so the caller declines to
// re-anchor rather than guess. Pure and extracted so the re-anchor rule
// is unit-testable without a live Machines API.
//
// The candidate filter is "same name AND currently attached" — not "no
// other volume shares this name at all". That looser check would be
// wrong here: a host migration leaves the pre-migration volume behind as
// a detached orphan that KEEPS the same name (a cluster can accumulate
// several same-named detached volumes over successive migrations), so
// requiring name-uniqueness across all volumes would permanently disable
// re-anchor exactly when it is needed. Attachment is the disambiguator:
// Fly enforces exclusive attachment (one machine per volume), so at most
// one same-named volume is attached at any instant — the live fork. The
// >1-attached guard below only trips in a genuinely misconfigured
// multi-volume-same-name topology (the documented "don't set this flag
// there" case), and there we decline rather than guess.
func pickReplacementVolume(vols []flyio.VolumeInfo, name, trackedID string) *flyio.VolumeInfo {
	var match *flyio.VolumeInfo
	for i := range vols {
		v := &vols[i]
		if v.ID == trackedID || v.Name != name || v.AttachedMachineID == "" {
			continue
		}
		if match != nil {
			return nil // ambiguous — more than one live same-named volume
		}
		match = v
	}
	return match
}

func (r *volumeResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan volumeResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var state volumeResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client := r.pd.client
	appName := plan.App.ValueString()

	// Operate on the planned id. It normally equals state.ID, but after a
	// plan-time host-migration re-anchor (see ModifyPlan) plan.ID is the fork
	// and state.ID is the stale, already-destroyed volume — reading that back
	// would fail with "volume disappeared during update".
	volumeID := plan.ID.ValueString()
	if plan.ID.IsUnknown() || plan.ID.IsNull() {
		volumeID = state.ID.ValueString()
	}

	// Size change → ExtendVolume. Shrinks are rejected at plan time in
	// ModifyPlan, so any size delta reaching Update is an increase.
	if plan.SizeGB.ValueInt64() != state.SizeGB.ValueInt64() {
		_, err := client.ExtendVolume(ctx, appName, volumeID, int(plan.SizeGB.ValueInt64()))
		if err != nil {
			resp.Diagnostics.AddError("Extend Volume Error", err.Error())
			return
		}
	}

	// Non-size mutable fields: auto_backup_enabled, snapshot_retention.
	var autoBackup *bool
	var snapshotRetention *int
	if plan.AutoBackupEnabled.ValueBool() != state.AutoBackupEnabled.ValueBool() {
		v := plan.AutoBackupEnabled.ValueBool()
		autoBackup = &v
	}
	if !plan.SnapshotRetention.IsNull() && plan.SnapshotRetention.ValueInt64() != state.SnapshotRetention.ValueInt64() {
		v := int(plan.SnapshotRetention.ValueInt64())
		snapshotRetention = &v
	}
	if autoBackup != nil || snapshotRetention != nil {
		_, err := client.UpdateVolume(ctx, appName, volumeID, autoBackup, snapshotRetention)
		if err != nil {
			resp.Diagnostics.AddError("Update Volume Error", err.Error())
			return
		}
	}

	// Read back authoritative state (size, zone, attached machine may have shifted).
	info, err := client.GetVolume(ctx, appName, volumeID)
	if err != nil {
		resp.Diagnostics.AddError("Read Volume Error", err.Error())
		return
	}
	if info == nil {
		resp.Diagnostics.AddError("Read Volume Error", "volume disappeared during update")
		return
	}

	setVolumeState(&plan, info)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *volumeResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state volumeResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client := r.pd.client
	if err := client.DeleteVolume(ctx, state.App.ValueString(), state.ID.ValueString()); err != nil {
		resp.Diagnostics.AddError("Delete Volume Error", err.Error())
	}
}

func (r *volumeResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Import ID format: "<app>/<volume_id>"
	for i := range req.ID {
		if req.ID[i] == '/' {
			app := req.ID[:i]
			id := req.ID[i+1:]
			resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("app"), app)...)
			resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id)...)
			return
		}
	}
	resp.Diagnostics.AddError("Invalid Import ID",
		"Expected '<app>/<volume_id>', got "+req.ID)
}

func setVolumeState(m *volumeResourceModel, info *flyio.VolumeInfo) {
	m.ID = types.StringValue(info.ID)
	m.Name = types.StringValue(info.Name)
	m.Region = types.StringValue(info.Region)
	m.SizeGB = types.Int64Value(int64(info.SizeGB))
	m.Encrypted = types.BoolValue(info.Encrypted)
	m.Fstype = types.StringValue(info.Fstype)
	m.AutoBackupEnabled = types.BoolValue(info.AutoBackupEnabled)
	m.SnapshotRetention = types.Int64Value(int64(info.SnapshotRetention))
	m.State = types.StringValue(info.State)
	m.AttachedMachineID = types.StringValue(info.AttachedMachineID)
	m.Zone = types.StringValue(info.Zone)
}
