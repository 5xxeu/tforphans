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

	"github.com/hashicorp/terraform-plugin-framework/datasource"
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

func (p *fakeProvider) DataSources(context.Context) []func() datasource.DataSource { return nil }

func (p *fakeProvider) Resources(context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		func() resource.Resource { return &thing{kind: "thing"} },
		func() resource.Resource { return &thing{kind: "broken"} },
		func() resource.Resource { return &thing{kind: "zoned"} },
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
	if t.kind == "broken" {
		var d diag.Diagnostics
		d.AddError("access denied", "fake_broken always fails to list")
		stream.Results = list.ListResultsStreamDiagnostics(d)
		return
	}
	entries, _ := os.ReadDir(t.dir())
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
				r.Diagnostics.Append(r.Resource.Set(ctx, thingModel{ID: types.StringValue(n), Name: types.StringValue(n)})...)
			}
			if !push(r) {
				return
			}
		}
	}
}
