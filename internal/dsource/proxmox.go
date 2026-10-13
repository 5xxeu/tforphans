package dsource

import (
	"regexp"
	"strings"

	"github.com/5xxeu/tforphans/internal/model"
)

// Proxmox covers bpg/proxmox. Its plural data sources list cluster-wide
// objects; guests are matched by VM ID, which is unique in a cluster and
// survives migration between nodes. telmate/proxmox resources count as
// managing the same guests and pools.
//
// State does not record the cluster, and VM IDs repeat across clusters. A
// guest never matches state whose node the scanned cluster does not have
// (any of its nodes counts: guests migrate). A node it has proves nothing,
// since node names (pve, pve1) repeat too, so matches stay unverified unless
// the user states with -proxmox-cluster-states that the states given are
// this cluster's. Cluster-wide objects (pools, users, roles, HA, ...) have
// no node at all.
var Proxmox = Adapter{
	Name:     "proxmox",
	Provider: "registry.terraform.io/bpg/proxmox",
	Target:   "cluster",
	Lists: map[string]ListSpec{
		"proxmox_virtual_environment_vms":           {Items: "vms", Field: "vm_id", Key: "vm_id"},
		"proxmox_virtual_environment_containers":    {Items: "containers", Field: "vm_id", Key: "vm_id"},
		"proxmox_virtual_environment_pools":         {IDs: "pool_ids", Key: "pool_id"},
		"proxmox_virtual_environment_users":         {IDs: "user_ids", Key: "user_id"},
		"proxmox_virtual_environment_groups":        {IDs: "group_ids", Key: "group_id"},
		"proxmox_virtual_environment_roles":         {IDs: "role_ids", Key: "role_id"},
		"proxmox_virtual_environment_hagroups":      {IDs: "group_ids", Key: "group"},
		"proxmox_virtual_environment_haresources":   {IDs: "resource_ids", Key: "resource_id"},
		"proxmox_virtual_environment_acme_accounts": {IDs: "accounts", Key: "name"},
		"proxmox_virtual_environment_replications":  {Items: "replications"},
		"proxmox_virtual_environment_sdn_vnets":     {Items: "vnets"},
		// Data sources of the new resource names.
		"proxmox_hagroups":      {IDs: "group_ids", Key: "group"},
		"proxmox_haresources":   {IDs: "resource_ids", Key: "resource_id"},
		"proxmox_acme_accounts": {IDs: "accounts", Key: "name"},
		"proxmox_replications":  {Items: "replications"},
		"proxmox_sdn_vnets":     {Items: "vnets"},
		"proxmox_backup_jobs":   {Items: "jobs"},
	},
	AltTypes: map[string][]string{
		"proxmox_virtual_environment_vm":        {"proxmox_vm", "proxmox_cloned_vm", "proxmox_vm_qemu"},
		"proxmox_virtual_environment_container": {"proxmox_lxc"},
	},
	AltIdentity: func(resourceType string, item map[string]any) map[string]map[string]any {
		switch resourceType {
		case "proxmox_virtual_environment_vm":
			// proxmox_vm keys the VM by its numeric id, telmate by vmid.
			return map[string]map[string]any{
				"proxmox_vm":        {"id": item["vm_id"]},
				"proxmox_cloned_vm": {"id": item["vm_id"]},
				"proxmox_vm_qemu":   {"vmid": item["vm_id"]},
			}
		case "proxmox_virtual_environment_container":
			return map[string]map[string]any{"proxmox_lxc": {"vmid": item["vm_id"]}}
		}
		return nil
	},
	Keep: func(resourceType string, item map[string]any) bool {
		switch resourceType {
		case "proxmox_virtual_environment_role":
			// Built-in roles (Administrator, PVEAdmin, ...).
			return item["special"] != true
		case "proxmox_virtual_environment_user":
			return item["id"] != "root@pam"
		}
		return true
	},
	ImportID: func(resourceType string, item map[string]any, id string) string {
		switch resourceType {
		case "proxmox_virtual_environment_vm", "proxmox_virtual_environment_container":
			if node, ok := item["node_name"].(string); ok && node != "" {
				return node + "/" + id
			}
		}
		return id
	},
	// The version is one cheap request: wrong credentials fail it once,
	// not once per data source.
	Probe:      "proxmox_virtual_environment_version",
	AuthFailed: func(message string) bool { return proxmoxAuthFailed.MatchString(message) },
	// Proxmox VE 9 migrated HA groups to HA rules and answers 500.
	Unsupported: func(dsType, message string) string {
		if strings.HasSuffix(dsType, "_hagroups") && strings.Contains(strings.ToLower(message), "migrated to rules") {
			return "the cluster has HA rules instead of HA groups (Proxmox VE 9); HA rules have no plural data source"
		}
		return ""
	},
	// Template and VM names repeat per node.
	DisplayName: func(resourceType string, item map[string]any, name string) string {
		if node, ok := item["node_name"].(string); ok && node != "" {
			return name + " (" + node + ")"
		}
		return name
	},
	Prepare: proxmoxRenames,
	Context: []string{"proxmox_virtual_environment_nodes"},
	Finish:  proxmoxScope,
}

// proxmoxAuthFailed matches a rejected login: bpg reports "received an
// HTTP 401 response - Reason: ..." (the reason is Proxmox VE's own text,
// such as "authentication failure" or "invalid token value!"), and a 401
// alone in an ID or a URL must not count.
var proxmoxAuthFailed = regexp.MustCompile(`(?i)\bHTTP 401\b|\bstatus(?: ?code)?:? 401\b|\b401 unauthorized\b|authentication failure|failed to authenticate`)

// proxmoxGuestNodes are the state attributes holding a guest's node.
var proxmoxGuestNodes = map[string]string{
	"proxmox_virtual_environment_vm":        "node_name",
	"proxmox_virtual_environment_container": "node_name",
	"proxmox_vm":                            "node_name",
	"proxmox_cloned_vm":                     "node_name",
	"proxmox_vm_qemu":                       "target_node",
	"proxmox_lxc":                           "target_node",
}

func proxmoxScope(resources []model.Resource, context map[string]map[string]any) {
	nodes := map[string]bool{}
	names, _ := context["proxmox_virtual_environment_nodes"]["names"].([]any)
	for _, n := range names {
		if s, ok := n.(string); ok && s != "" {
			nodes[s] = true
		}
	}
	for i := range resources {
		r := &resources[i]
		switch r.Type {
		case "proxmox_virtual_environment_vm", "proxmox_virtual_environment_container":
			if len(nodes) > 0 {
				r.Scope = &model.Scope{Attrs: proxmoxGuestNodes, Values: nodes}
				continue
			}
		}
		r.Scope = &model.Scope{Unverifiable: true}
	}
}

// proxmoxRenames pairs the deprecated proxmox_virtual_environment_<x> types
// with their proxmox_<x> successors: the object is listed once, under the
// successor when it has a plural data source, and either type in state
// manages it.
func proxmoxRenames(deprecated map[string]bool, hasPlural func(string) bool) (map[string]string, map[string][]string) {
	skip := map[string]string{}
	alt := map[string][]string{}
	const old = "proxmox_virtual_environment_"
	for t, dep := range deprecated {
		rest, ok := strings.CutPrefix(t, old)
		if !ok || !dep {
			continue
		}
		successor := "proxmox_" + rest
		if _, exists := deprecated[successor]; !exists {
			continue
		}
		if hasPlural(successor) {
			skip[t] = "listed as " + successor
			alt[successor] = append(alt[successor], t)
		} else {
			alt[t] = append(alt[t], successor)
		}
	}
	return skip, alt
}
