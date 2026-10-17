package state

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/5xxeu/tforphans/internal/model"
)

// Snapshot retains the instances and provenance of one state read.
type Snapshot struct {
	Lineage   string
	Serial    *int64
	Instances []Instance
	Sources   []string
}

// ParseShowSnapshot reads refresh output, which has no lineage or serial.
func ParseShowSnapshot(data []byte, source string) (Snapshot, error) {
	instances, err := ParseShow(data)
	return Snapshot{Instances: instances, Sources: []string{source}}, err
}

// AddSnapshots indexes all instances for live matching and merges equivalent
// snapshots for ownership analysis. Legacy attributes still matter to matching.
func (ix *Index) AddSnapshots(snapshots []Snapshot) {
	if ix.snapshotKeys == nil {
		ix.snapshotKeys = map[string]int{}
	}
	for _, s := range snapshots {
		// Where each instance comes from, for -explain; the snapshot keeps
		// its instances as read.
		from := strings.Join(sortedSources(slices.Clone(s.Sources)), ", ")
		instances := slices.Clone(s.Instances)
		for i := range instances {
			if instances[i].Source == "" {
				instances[i].Source = from
			}
		}
		ix.Add(instances)
		rows := make([]string, 0, len(s.Instances))
		for _, in := range s.Instances {
			kind, value := objectKey(in)
			rows = append(rows, canonical([]any{in.Type, providerFamily(in.Provider), in.Address, kind, value}))
		}
		slices.Sort(rows)
		rows = slices.Compact(rows)
		serial := ""
		if s.Lineage != "" && s.Serial != nil {
			serial = fmt.Sprint(*s.Serial)
		}
		key := canonical([]any{s.Lineage, serial, fmt.Sprintf("%x", sha256.Sum256([]byte(canonical(rows))))})
		if s.Lineage == "" {
			// Without a lineage, equal content proves nothing: copied roots
			// with separate states look the same. Only a repeated read of
			// the same source merges.
			key = canonical([]any{key, sortedSources(slices.Clone(s.Sources))})
		}
		if i, ok := ix.snapshotKeys[key]; ok {
			ix.snapshots[i].Sources = sortedSources(append(ix.snapshots[i].Sources, s.Sources...))
			// Equivalent identities can still carry different legacy IDs.
			instanceKey := func(in Instance) string {
				kind, value := objectKey(in)
				return canonical([]any{in.Type, providerFamily(in.Provider), in.Address, kind, value, in.Attributes["id"]})
			}
			instances := map[string]int{}
			for j, in := range ix.snapshots[i].Instances {
				instances[instanceKey(in)] = j
			}
			for _, in := range s.Instances {
				key := instanceKey(in)
				if j, ok := instances[key]; ok {
					stored := &ix.snapshots[i].Instances[j]
					stored.IDSensitive = stored.IDSensitive || in.IDSensitive
				} else {
					instances[key] = len(ix.snapshots[i].Instances)
					ix.snapshots[i].Instances = append(ix.snapshots[i].Instances, in)
				}
			}
			continue
		}
		s.Instances = slices.Clone(s.Instances)
		s.Sources = sortedSources(slices.Clone(s.Sources))
		ix.snapshotKeys[key] = len(ix.snapshots)
		ix.snapshots = append(ix.snapshots, s)
	}
}

func sortedSources(sources []string) []string {
	slices.Sort(sources)
	return slices.Compact(sources)
}

func providerFamily(provider string) string {
	if provider == "" {
		return ""
	}
	return model.ProviderFamily(provider)
}

func canonical(v any) string {
	b, _ := json.Marshal(v) // State values have already passed JSON decoding.
	return string(b)
}

func objectKey(in Instance) (string, string) {
	if len(in.Identity) > 0 {
		if !fullIdentity(in.Identity) {
			return "identity", canonical([]any{in.Identity, in.Attributes["id"]})
		}
		return "identity", canonical(in.Identity)
	}
	return "id", canonical(in.Attributes["id"])
}

// These providers create no remote object; equal IDs can be legitimate.
var noRemoteObject = map[string]bool{
	"hashicorp/null": true, "hashicorp/random": true, "hashicorp/time": true,
	"hashicorp/local": true, "hashicorp/tls": true, "hashicorp/archive": true,
	"hashicorp/external": true, "hashicorp/http": true, "hashicorp/cloudinit": true,
}

func excludedOwnership(in Instance) bool {
	return in.Type == "terraform_data" || noRemoteObject[providerFamily(in.Provider)]
}

// Owner is an address and the logical state that contains it.
type Owner struct {
	Address string   `json:"address"`
	Lineage string   `json:"lineage,omitempty"`
	Serial  *int64   `json:"serial,omitempty"`
	Sources []string `json:"sources"`
}

// Duplicate describes one object with multiple state owners or addresses.
type Duplicate struct {
	Type        string         `json:"type"`
	Provider    string         `json:"provider"`
	Identity    map[string]any `json:"identity,omitempty"`
	ID          any            `json:"id,omitempty"`
	IDSensitive bool           `json:"id_sensitive,omitempty"`
	Confidence  string         `json:"confidence"`
	Reason      string         `json:"reason,omitempty"`
	Owners      []Owner        `json:"owners"`
}

type objectOwner struct {
	state   int
	address string
}

type stateObject struct {
	instance  Instance
	owners    []objectOwner
	known     map[string]string
	legacyIDs map[string]bool // canonical JSON ID to sensitivity
	full      bool
}

func (o *stateObject) addLegacyID(in Instance) {
	if id := in.Attributes["id"]; knownValue(id) {
		if o.legacyIDs == nil {
			o.legacyIDs = map[string]bool{}
		}
		key := canonical(id)
		o.legacyIDs[key] = o.legacyIDs[key] || in.IDSensitive
	}
}

// identityValues excludes unknown values, including empty containers.
func identityValues(id map[string]any) map[string]string {
	values := map[string]string{}
	for k, v := range id {
		if knownValue(v) {
			values[k] = canonical(v)
		}
	}
	return values
}

func fullIdentity(id map[string]any) bool {
	if len(id) == 0 {
		return false
	}
	for _, value := range id {
		if !knownValue(value) {
			return false
		}
	}
	return true
}

func knownValue(v any) bool {
	switch v := v.(type) {
	case nil:
		return false
	case string:
		return v != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	default:
		return true
	}
}

type identityValue struct {
	key   string
	value string
}

type identityGroup struct {
	known  []string
	full   bool
	values map[identityValue][]int // key and canonical JSON value
}

func (g *identityGroup) smallestBucket(o stateObject) []int {
	var smallest []int
	shared := false
	for _, k := range g.known {
		v, ok := o.known[k]
		if !ok {
			continue
		}
		bucket := g.values[identityValue{k, v}]
		if !shared || len(bucket) < len(smallest) {
			smallest, shared = bucket, true
		}
	}
	return smallest
}

func sharedIdentityGroups(o stateObject, byKey map[string][]*identityGroup) map[*identityGroup]bool {
	groups := map[*identityGroup]bool{}
	for k := range o.known {
		for _, g := range byKey[k] {
			groups[g] = true
		}
	}
	return groups
}

// Return the sole compatible grouped object, -1 for none, or -2 for ambiguity.
// Counting stops at two, even when a scope value has a very large bucket.
func uniqueIdentityCandidate(i int, objects []stateObject, groups map[*identityGroup]bool) int {
	o := objects[i]
	candidate := -1
	for g := range groups {
		for _, j := range g.smallestBucket(o) {
			if j == i {
				continue
			}
			other := objects[j]
			if len(o.owners) == 1 && len(other.owners) == 1 && o.owners[0] == other.owners[0] {
				continue
			}
			if ok, _ := objectEquality(o, other); !ok {
				continue
			}
			if candidate >= 0 {
				return -2
			}
			candidate = j
		}
	}
	return candidate
}

func identityCompleteness(a, b stateObject) int {
	if a.full != b.full {
		if a.full {
			return 1
		}
		return -1
	}
	return len(a.known) - len(b.known)
}

// Duplicates compares states by indexed identity values. Identical objects
// are grouped first, so repeated addresses do not multiply ownership rows.
func (ix *Index) Duplicates() (ownership, inState []Duplicate) {
	var objects []stateObject
	byObject := map[string]int{}
	byPayload := map[string][]int{}
	for si, s := range ix.snapshots {
		seen := map[string]bool{}
		for _, in := range s.Instances {
			if excludedOwnership(in) {
				continue
			}
			kind, value := objectKey(in)
			if kind == "id" && !knownValue(in.Attributes["id"]) {
				continue
			}
			key := canonical([]string{in.Type, providerFamily(in.Provider), kind, value})
			if len(in.Identity) > 0 && !fullIdentity(in.Identity) && !knownValue(in.Attributes["id"]) {
				key = canonical([]any{key, si, in.Address})
				value = canonical([]any{value, si, in.Address})
			}
			addressKey := canonical([]string{key, in.Address})
			i, ok := byObject[key]
			if !ok {
				i = len(objects)
				byObject[key] = i
				payload := canonical([]string{in.Type, kind, value})
				byPayload[payload] = append(byPayload[payload], i)
				known := identityValues(in.Identity)
				objects = append(objects, stateObject{instance: in, known: known, full: len(in.Identity) > 0 && len(known) == len(in.Identity)})
			}
			objects[i].addLegacyID(in)
			objects[i].instance.IDSensitive = objects[i].instance.IDSensitive || in.IDSensitive
			if !seen[addressKey] {
				objects[i].owners = append(objects[i].owners, objectOwner{si, in.Address})
				seen[addressKey] = true
			}
		}
	}
	// An unknown provider can join an exact object when only one known
	// family holds it; it must not join unrelated known provider families.
	for _, indexes := range byPayload {
		unknown, known, families := -1, -1, 0
		for _, i := range indexes {
			if objects[i].instance.Provider == "" {
				unknown = i
			} else {
				known = i
				families++
			}
		}
		if unknown >= 0 && families == 1 {
			objects[known].instance.IDSensitive = objects[known].instance.IDSensitive || objects[unknown].instance.IDSensitive
			for id, sensitive := range objects[unknown].legacyIDs {
				if objects[known].legacyIDs == nil {
					objects[known].legacyIDs = map[string]bool{}
				}
				objects[known].legacyIDs[id] = objects[known].legacyIDs[id] || sensitive
			}
			seen := map[objectOwner]bool{}
			for _, o := range objects[known].owners {
				seen[o] = true
			}
			for _, o := range objects[unknown].owners {
				if !seen[o] {
					objects[known].owners = append(objects[known].owners, o)
					seen[o] = true
				}
			}
			objects[unknown].owners = nil
		}
	}
	objects = slices.DeleteFunc(objects, func(o stateObject) bool { return len(o.owners) == 0 })
	// Group by known key set, then use the smallest shared value bucket.
	// A shared account or region alone must not force a scan of every ID.
	groups := map[string]map[string]*identityGroup{}
	groupsByKey := map[string]map[string][]*identityGroup{}
	byID := map[identityValue][]int{}
	legacyID := map[identityValue][]int{}
	for i, o := range objects {
		typ := o.instance.Type
		for id := range o.legacyIDs {
			key := identityValue{typ, id}
			byID[key] = append(byID[key], i)
			if len(o.instance.Identity) == 0 {
				legacyID[key] = append(legacyID[key], i)
			}
		}
		if len(o.instance.Identity) == 0 {
			continue
		}
		keys := model.SortedKeys(o.instance.Identity)
		known := slices.DeleteFunc(slices.Clone(keys), func(k string) bool { _, ok := o.known[k]; return !ok })
		shape := canonical([]any{keys, known})
		if groups[typ] == nil {
			groups[typ] = map[string]*identityGroup{}
			groupsByKey[typ] = map[string][]*identityGroup{}
		}
		g := groups[typ][shape]
		if g == nil {
			g = &identityGroup{known: known, full: o.full, values: map[identityValue][]int{}}
			groups[typ][shape] = g
			for _, k := range known {
				groupsByKey[typ][k] = append(groupsByKey[typ][k], g)
			}
		}
		for k, v := range o.known {
			key := identityValue{k, v}
			g.values[key] = append(g.values[key], i)
		}
	}
	var matches [][2]int
	partialCandidates := map[int]int{}
	uniqueCandidate := func(i int) int {
		if j, ok := partialCandidates[i]; ok {
			return j
		}
		o := objects[i]
		j := uniqueIdentityCandidate(i, objects, sharedIdentityGroups(o, groupsByKey[o.instance.Type]))
		partialCandidates[i] = j
		return j
	}
	for i, o := range objects {
		if len(o.owners) > 1 {
			if _, reason := objectEquality(o, o); reason != "partial_identity" || uniqueCandidate(i) != -2 {
				matches = append(matches, [2]int{i, i})
			}
		}
		candidates := map[int]bool{}
		candidateGroups := sharedIdentityGroups(o, groupsByKey[o.instance.Type])
		needsPartial := !o.full && len(o.known) > 0
		keys := model.SortedKeys(o.instance.Identity)
		for g := range candidateGroups {
			if !o.full || !g.full {
				continue
			}
			if !slices.Equal(keys, g.known) {
				needsPartial = needsPartial || len(o.known) <= len(g.known)
				continue
			}
			// Exact full identities retain their existing confirmed matching.
			for _, j := range g.smallestBucket(o) {
				if j > i {
					candidates[j] = true
				}
			}
		}
		if needsPartial {
			if j := uniqueCandidate(i); j >= 0 {
				completeness := identityCompleteness(o, objects[j])
				if completeness < 0 || completeness == 0 && j > i && uniqueCandidate(j) == i {
					candidates[j] = true
				}
			}
		}
		for id := range o.legacyIDs {
			key := identityValue{o.instance.Type, id}
			bucket := legacyID[key]
			if len(o.instance.Identity) == 0 {
				bucket = byID[key]
			}
			for _, j := range bucket {
				if j > i && (len(o.instance.Identity) == 0 || len(objects[j].instance.Identity) == 0) {
					candidates[j] = true
				}
			}
		}
		for j := range candidates {
			if ok, _ := objectEquality(o, objects[j]); ok {
				matches = append(matches, [2]int{i, j})
			}
		}
	}
	// Resolve compatible objects before recording any reason groups. A partial
	// object's self-match and its fuller match must share one report key.
	parents := make([]int, len(objects))
	for i := range parents {
		parents[i] = i
	}
	find := func(i int) int {
		for parents[i] != i {
			parents[i] = parents[parents[i]]
			i = parents[i]
		}
		return i
	}
	for _, pair := range matches {
		if _, reason := objectEquality(objects[pair[0]], objects[pair[1]]); reason != "partial_identity" {
			continue
		}
		a, b := find(pair[0]), find(pair[1])
		if preferObject(objects[b], objects[a]) {
			a, b = b, a
		}
		parents[b] = a
	}
	for i, o := range objects {
		if o.instance.IDSensitive {
			objects[find(i)].instance.IDSensitive = true
		}
	}
	results := duplicateResults{ix: ix, ownership: map[string]*Duplicate{}, inState: map[string]*Duplicate{}, ownerSets: map[*Duplicate]map[objectOwner]bool{}}
	for _, pair := range matches {
		a, b := objects[find(pair[0])], objects[find(pair[1])]
		if preferObject(b, a) {
			a, b = b, a
		}
		in := a.instance
		in.IDSensitive = in.IDSensitive || b.instance.IDSensitive
		results.add(objects[pair[0]], objects[pair[1]], in)
	}
	return sortedDuplicates(results.ownership), sortedDuplicates(results.inState)
}

func preferObject(a, b stateObject) bool {
	if c := identityCompleteness(a, b); c != 0 {
		return c > 0
	}
	if (a.instance.Provider == "") != (b.instance.Provider == "") {
		return a.instance.Provider != ""
	}
	aKind, aValue := objectKey(a.instance)
	bKind, bValue := objectKey(b.instance)
	return canonical([]string{aKind, aValue}) < canonical([]string{bKind, bValue})
}

func objectEquality(a, b stateObject) (bool, string) {
	if !model.SameProvider(a.instance.Provider, b.instance.Provider) {
		return false, ""
	}
	if len(a.instance.Identity) == 0 || len(b.instance.Identity) == 0 {
		for id := range a.legacyIDs {
			if _, ok := b.legacyIDs[id]; ok {
				return true, "no_identity"
			}
		}
		return false, "no_identity"
	}
	overlap := 0
	for k, value := range a.known {
		if other, known := b.known[k]; known {
			if value != other {
				return false, ""
			}
			overlap++
		}
	}
	if overlap == 0 {
		return false, ""
	}
	if a.full && b.full && len(a.known) == len(b.known) && overlap == len(a.known) {
		return true, ""
	}
	return true, "partial_identity"
}

type duplicateResults struct {
	ix        *Index
	ownership map[string]*Duplicate
	inState   map[string]*Duplicate
	ownerSets map[*Duplicate]map[objectOwner]bool
}

func (r *duplicateResults) add(a, b stateObject, in Instance) {
	ok, reason := objectEquality(a, b)
	if !ok {
		return
	}
	kind, value := objectKey(in)
	object := canonical([]string{in.Type, providerFamily(in.Provider), kind, value})
	aStates, aLineages := groupOwners(a.owners, r.ix.snapshots)
	bStates, bLineages := groupOwners(b.owners, r.ix.snapshots)
	if reason == "" || reason == "no_identity" {
		for si, owners := range aStates {
			if others := bStates[si]; len(others) > 0 {
				all := append(slices.Clone(owners), others...)
				if distinctAddresses(all) > 1 {
					r.record(r.inState, canonical([]any{object, si, reason}), in, reason, all...)
				}
			}
		}
	}
	for lineage, owners := range aLineages {
		if lineage == "" || len(bLineages[lineage]) == 0 {
			continue
		}
		all := append(slices.Clone(owners), bLineages[lineage]...)
		if distinctStates(all) > 1 {
			r.record(r.ownership, canonical([]string{object, "same_lineage_diverged", lineage}), in, "same_lineage_diverged", all...)
		}
	}
	for _, sides := range [][2][]objectOwner{{aLineages[""], b.owners}, {bLineages[""], a.owners}} {
		if len(sides[0]) == 0 {
			continue
		}
		all := append(slices.Clone(sides[0]), sides[1]...)
		if distinctStates(all) > 1 {
			r.record(r.ownership, canonical([]string{object, "lineage_unknown"}), in, "lineage_unknown", all...)
		}
	}
	for _, sides := range [][2]map[string][]objectOwner{{aLineages, bLineages}, {bLineages, aLineages}} {
		known := len(sides[1])
		if len(sides[1][""]) > 0 {
			known--
		}
		for lineage, owners := range sides[0] {
			if lineage != "" && (known > 1 || known == 1 && len(sides[1][lineage]) == 0) {
				r.record(r.ownership, canonical([]string{object, reason}), in, reason, owners...)
			}
		}
	}
}

func groupOwners(owners []objectOwner, snapshots []Snapshot) (map[int][]objectOwner, map[string][]objectOwner) {
	states := map[int][]objectOwner{}
	lineages := map[string][]objectOwner{}
	for _, o := range owners {
		states[o.state] = append(states[o.state], o)
		lineage := snapshots[o.state].Lineage
		lineages[lineage] = append(lineages[lineage], o)
	}
	return states, lineages
}

func distinctStates(owners []objectOwner) int {
	seen := map[int]bool{}
	for _, o := range owners {
		seen[o.state] = true
	}
	return len(seen)
}

func distinctAddresses(owners []objectOwner) int {
	seen := map[string]bool{}
	for _, o := range owners {
		seen[o.address] = true
	}
	return len(seen)
}

func (r *duplicateResults) record(dst map[string]*Duplicate, key string, in Instance, reason string, owners ...objectOwner) {
	d := dst[key]
	if d == nil {
		d = &Duplicate{Type: in.Type, Provider: providerFamily(in.Provider), Identity: in.Identity, Confidence: "confirmed", Reason: reason}
		if len(in.Identity) == 0 {
			d.ID = in.Attributes["id"]
		}
		if reason != "" {
			d.Confidence = "possible"
		}
		dst[key] = d
		r.ownerSets[d] = map[objectOwner]bool{}
	}
	if len(d.Identity) == 0 && in.IDSensitive {
		d.IDSensitive = true
	}
	for _, o := range owners {
		s := r.ix.snapshots[o.state]
		owner := Owner{Address: o.address, Lineage: s.Lineage, Serial: s.Serial, Sources: s.Sources}
		if !r.ownerSets[d][o] {
			r.ownerSets[d][o] = true
			d.Owners = append(d.Owners, owner)
		}
	}
}

func sortedDuplicates(entries map[string]*Duplicate) []Duplicate {
	type row struct {
		key       string
		duplicate Duplicate
	}
	rows := make([]row, 0, len(entries))
	for _, d := range entries {
		if d.IDSensitive {
			d.ID = "(sensitive)"
		}
		slices.SortFunc(d.Owners, compareOwners)
		rows = append(rows, row{canonical(d), *d})
	}
	slices.SortFunc(rows, func(a, b row) int { return strings.Compare(a.key, b.key) })
	var out []Duplicate
	for _, row := range rows {
		out = append(out, row.duplicate)
	}
	return out
}

func compareOwners(a, b Owner) int {
	if c := strings.Compare(a.Address, b.Address); c != 0 {
		return c
	}
	if c := strings.Compare(a.Lineage, b.Lineage); c != 0 {
		return c
	}
	if a.Serial == nil && b.Serial != nil {
		return -1
	}
	if a.Serial != nil && b.Serial == nil {
		return 1
	}
	if a.Serial != nil && b.Serial != nil {
		if *a.Serial < *b.Serial {
			return -1
		}
		if *a.Serial > *b.Serial {
			return 1
		}
	}
	return slices.Compare(a.Sources, b.Sources)
}
