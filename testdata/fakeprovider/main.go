// Command terraform-provider-fake is a minimal provider used by the
// integration tests. Every "thing" is a file in $FAKE_DIR; the provider
// supports resource identity and list resources, so `terraform query` works
// without any cloud credentials.
package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	dsschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/list"
	listschema "github.com/hashicorp/terraform-plugin-framework/list/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/identityschema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func main() {
	err := providerserver.Serve(context.Background(), func() provider.Provider { return &fakeProvider{} },
		providerserver.ServeOpts{Address: "example.com/fake/fake"})
	if err != nil {
		log.Fatal(err)
	}
}

type fakeProvider struct{}

var (
	_ provider.Provider                  = (*fakeProvider)(nil)
	_ provider.ProviderWithListResources = (*fakeProvider)(nil)
)

func (p *fakeProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "fake"
}

func (p *fakeProvider) Schema(context.Context, provider.SchemaRequest, *provider.SchemaResponse) {}

func (p *fakeProvider) Configure(context.Context, provider.ConfigureRequest, *provider.ConfigureResponse) {
}

func (p *fakeProvider) DataSources(context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		func() datasource.DataSource { return &plural{kind: "zone"} },
		func() datasource.DataSource { return &plural{kind: "record"} },
	}
}

func (p *fakeProvider) Resources(context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		func() resource.Resource { return &thing{kind: "thing"} },
		func() resource.Resource { return &thing{kind: "broken"} },
		func() resource.Resource { return &thing{kind: "zoned"} },
		func() resource.Resource { return &thing{kind: "zone"} },
		func() resource.Resource { return &thing{kind: "record"} },
	}
}

func (p *fakeProvider) ListResources(context.Context) []func() list.ListResource {
	return []func() list.ListResource{
		func() list.ListResource { return &thing{kind: "thing"} },
		func() list.ListResource { return &thing{kind: "broken"} },
		func() list.ListResource { return &thing{kind: "zoned"} },
	}
}

// thing implements fake_thing, fake_broken (List always fails) and
// fake_zoned (list config has a required argument).
type thing struct{ kind string }

type thingModel struct {
	ID   types.String `tfsdk:"id"`
	Name types.String `tfsdk:"name"`
	Tags types.Map    `tfsdk:"tags"`
}

type thingIdentity struct {
	ID types.String `tfsdk:"id"`
}

func (t *thing) dir() string { return filepath.Join(os.Getenv("FAKE_DIR"), t.kind) }

func (t *thing) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_" + t.kind
}

func (t *thing) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{Attributes: map[string]schema.Attribute{
		"id":   schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
		"name": schema.StringAttribute{Required: true, PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
		"tags": schema.MapAttribute{ElementType: types.StringType, Optional: true},
	}}
}

func (t *thing) IdentitySchema(_ context.Context, _ resource.IdentitySchemaRequest, resp *resource.IdentitySchemaResponse) {
	resp.IdentitySchema = identityschema.Schema{Attributes: map[string]identityschema.Attribute{
		"id": identityschema.StringAttribute{RequiredForImport: true},
	}}
}

func (t *thing) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m thingModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := os.MkdirAll(t.dir(), 0o755); err != nil {
		resp.Diagnostics.AddError("mkdir", err.Error())
		return
	}
	if err := os.WriteFile(filepath.Join(t.dir(), m.Name.ValueString()), nil, 0o644); err != nil {
		resp.Diagnostics.AddError("write", err.Error())
		return
	}
	m.ID = m.Name
	resp.Diagnostics.Append(resp.State.Set(ctx, m)...)
	resp.Diagnostics.Append(resp.Identity.Set(ctx, thingIdentity{ID: m.ID})...)
}

func (t *thing) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m thingModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if _, err := os.Stat(filepath.Join(t.dir(), m.ID.ValueString())); os.IsNotExist(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	m.Name = m.ID
	resp.Diagnostics.Append(resp.State.Set(ctx, m)...)
	resp.Diagnostics.Append(resp.Identity.Set(ctx, thingIdentity{ID: m.ID})...)
}

func (t *thing) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m thingModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, m)...)
}

func (t *thing) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m thingModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	_ = os.Remove(filepath.Join(t.dir(), m.ID.ValueString()))
}

func (t *thing) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughWithIdentity(ctx, path.Root("id"), path.Root("id"), req, resp)
}

func (t *thing) ListResourceConfigSchema(_ context.Context, _ list.ListResourceSchemaRequest, resp *list.ListResourceSchemaResponse) {
	attrs := map[string]listschema.Attribute{}
	if t.kind == "zoned" {
		attrs["zone"] = listschema.StringAttribute{Required: true}
	}
	resp.Schema = listschema.Schema{Attributes: attrs}
}

func (t *thing) List(ctx context.Context, req list.ListRequest, stream *list.ListResultsStream) {
	// FAKE_PANIC=<kind> crashes the provider while listing that kind.
	if os.Getenv("FAKE_PANIC") == t.kind {
		panic("fake provider crash")
	}
	// FAKE_HANG=<kind> writes the provider's pid to $FAKE_DIR/hanging and
	// never finishes listing that kind, for tests that stop a run.
	if os.Getenv("FAKE_HANG") == t.kind {
		_ = os.WriteFile(filepath.Join(os.Getenv("FAKE_DIR"), "hanging"), []byte(strconv.Itoa(os.Getpid())), 0o644)
		<-ctx.Done()
		time.Sleep(time.Hour)
	}
	// FAKE_FAIL=<kind>:<message> makes listing that kind fail with message.
	if kind, msg, ok := strings.Cut(os.Getenv("FAKE_FAIL"), ":"); ok && kind == t.kind {
		var d diag.Diagnostics
		d.AddError("Error listing", msg)
		stream.Results = list.ListResultsStreamDiagnostics(d)
		return
	}
	if t.kind == "broken" {
		var d diag.Diagnostics
		d.AddError("access denied", "fake_broken always fails to list")
		stream.Results = list.ListResultsStreamDiagnostics(d)
		return
	}
	dir := t.dir()
	// fake_zoned lists the files of $FAKE_DIR/zoned/<zone>.
	if t.kind == "zoned" {
		var zone types.String
		if d := req.Config.GetAttribute(ctx, path.Root("zone"), &zone); !d.HasError() {
			dir = filepath.Join(dir, zone.ValueString())
		}
	}
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	stream.Results = func(push func(list.ListResult) bool) {
		for _, n := range names {
			r := req.NewListResult(ctx)
			r.DisplayName = n
			r.Diagnostics.Append(r.Identity.Set(ctx, thingIdentity{ID: types.StringValue(n)})...)
			if req.IncludeResource {
				// A Name tag that differs from the display name, for
				// adopt -names-from-tags.
				tags := types.MapValueMust(types.StringType, map[string]attr.Value{"Name": types.StringValue("Tagged " + n)})
				r.Diagnostics.Append(r.Resource.Set(ctx, thingModel{ID: types.StringValue(n), Name: types.StringValue(n), Tags: tags})...)
			}
			if !push(r) {
				return
			}
		}
	}
}

// plural implements the data sources fake_zones (a file per zone in
// $FAKE_DIR/zone) and fake_records (files in $FAKE_DIR/record/<zone_id>),
// shaped like cloudflare 5.x: result[] of objects with an id.
// FAKE_PANIC=record:<zone> crashes the provider reading that zone's records,
// record:<zone>:<type> only when reading that type. A record's type is the
// content of its file ("A" when empty). FAKE_MIXED=<zone> crashes reading
// that zone's records unless a type is given, like Cloudflare's
// "inconsistent list element types".
type plural struct{ kind string }

var itemType = map[string]attr.Type{"id": types.StringType, "name": types.StringType}

func (p *plural) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_" + p.kind + "s"
}

func (p *plural) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	attrs := map[string]dsschema.Attribute{
		"result": dsschema.ListNestedAttribute{Computed: true, NestedObject: dsschema.NestedAttributeObject{Attributes: map[string]dsschema.Attribute{
			"id":   dsschema.StringAttribute{Computed: true},
			"name": dsschema.StringAttribute{Computed: true},
		}}},
	}
	if p.kind == "record" {
		attrs["zone_id"] = dsschema.StringAttribute{Optional: true}
		attrs["type"] = dsschema.StringAttribute{Optional: true, Description: "Record type.\nAvailable values: \"A\", \"CAA\", \"NAPTR\"."}
	}
	resp.Schema = dsschema.Schema{Attributes: attrs}
}

func (p *plural) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	dir := filepath.Join(os.Getenv("FAKE_DIR"), p.kind)
	var zone, typ types.String
	if p.kind == "record" {
		resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("zone_id"), &zone)...)
		resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("type"), &typ)...)
		dir = filepath.Join(dir, zone.ValueString())
		if v := os.Getenv("FAKE_PANIC"); v == "record:"+zone.ValueString() || v == "record:"+zone.ValueString()+":"+typ.ValueString() {
			panic("fake provider crash")
		}
		if os.Getenv("FAKE_MIXED") == zone.ValueString() && typ.IsNull() {
			panic("inconsistent list element types")
		}
	}
	// FAKE_FAIL=<kind>:<message> makes reading that data source fail.
	if kind, msg, ok := strings.Cut(os.Getenv("FAKE_FAIL"), ":"); ok && kind == p.kind {
		resp.Diagnostics.AddError("Error reading", msg)
		return
	}
	entries, _ := os.ReadDir(dir)
	var items []attr.Value
	for _, e := range entries {
		if p.kind == "record" && !typ.IsNull() {
			b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
			t := strings.TrimSpace(string(b))
			if t == "" {
				t = "A"
			}
			if t != typ.ValueString() {
				continue
			}
		}
		items = append(items, types.ObjectValueMust(itemType, map[string]attr.Value{
			"id": types.StringValue(e.Name()), "name": types.StringValue(e.Name() + ".name"),
		}))
	}
	list := types.ListValueMust(types.ObjectType{AttrTypes: itemType}, items)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("result"), list)...)
	if p.kind == "record" {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("zone_id"), zone)...)
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("type"), typ)...)
	}
}
