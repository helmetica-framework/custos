// Package gather holds the plan the controller writes and the `custos gather`
// Job reads, plus the resolution that Job performs. It is its own package
// rather than part of controllers because both sides need it: a plan written
// one way and read another would only show up in a cluster, never in a test.
package gather

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EntryKind is deliberately narrower than the API's SourceType. Only the two
// sources custos cannot resolve on its own ever reach the Job. Consts, claim
// params and templates are done in the controller and never leave it.
type EntryKind string

const (
	EntryObjectRef EntryKind = "objectRef"
	EntryExec      EntryKind = "exec"
)

// Entry is one value the Job has to resolve. Templating has already been
// applied by the controller, so every string here is literal.
//
// Kind says which fields apply. An objectRef entry sets APIVersion,
// ObjectKind, Name and exactly one of DataKey or Path. An exec entry sets
// PodSelector, Command and optionally Container.
type Entry struct {
	Key  string    `json:"key"`
	Kind EntryKind `json:"kind"`

	APIVersion string `json:"apiVersion,omitempty"`
	ObjectKind string `json:"objectKind,omitempty"`
	Name       string `json:"name,omitempty"`
	DataKey    string `json:"dataKey,omitempty"`
	Path       string `json:"path,omitempty"`

	PodSelector *metav1.LabelSelector `json:"podSelector,omitempty"`
	Container   string                `json:"container,omitempty"`
	Command     []string              `json:"command,omitempty"`
}

// Plan is everything the Job needs. It never names the Arcanum, because the
// Job only has to resolve entries in one namespace.
type Plan struct {
	Namespace string  `json:"namespace"`
	Entries   []Entry `json:"entries"`
}

// sortedEntries returns the entries in key order. Both callers need the same
// order for the same reason, that the controller builds Entries by ranging a
// map, and both work on a copy because the slice belongs to the caller.
func sortedEntries(entries []Entry) []Entry {
	out := slices.Clone(entries)

	slices.SortFunc(out, func(a, b Entry) int {
		return strings.Compare(a.Key, b.Key)
	})

	return out
}

// Marshal writes the plan as the JSON the Job reads out of its ConfigMap.
// So the plan is human readable.
func (p Plan) Marshal() ([]byte, error) {
	entries := sortedEntries(p.Entries)

	out, err := json.MarshalIndent(Plan{Namespace: p.Namespace, Entries: entries}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshalling plan: %w", err)
	}

	return out, nil
}

// Unmarshal reads a plan back out of the Job's ConfigMap. It does not sort.
// Marshal sorts for stable bytes, not to hand the Job an order it can lean
// on, so nothing downstream may assume Entries arrives sorted.
func Unmarshal(b []byte) (Plan, error) {
	var plan Plan

	err := json.Unmarshal(b, &plan)
	if err != nil {
		return Plan{}, fmt.Errorf("unmarshalling plan: %w", err)
	}

	return plan, nil
}

// Hash returns the eight hex characters that name the gather Job. The same
// plan has to hash the same on every run and in every process. A hash that
// moved would tear down and recreate the Job every reconcile, and since an
// exec entry is allowed to provision, that could mint a new password every
// few seconds.
func (p Plan) Hash() string {
	h := sha256.New()
	fmt.Fprintln(h, p.Namespace)

	entries := sortedEntries(p.Entries)

	for _, entry := range entries {
		fmt.Fprintln(h, entry.Key, entry.Kind, entry.APIVersion, entry.ObjectKind, entry.Name, entry.DataKey, entry.Path, entry.Container)

		for _, arg := range entry.Command {
			fmt.Fprintln(h, "arg", arg)
		}

		if entry.PodSelector != nil {
			for _, k := range slices.Sorted(maps.Keys(entry.PodSelector.MatchLabels)) {
				fmt.Fprintln(h, "label", k, entry.PodSelector.MatchLabels[k])
			}
			for _, m := range entry.PodSelector.MatchExpressions {
				fmt.Fprintln(h, "expr", m.Key, m.Operator, strings.Join(m.Values, ","))
			}
		}
	}

	return hex.EncodeToString(h.Sum(nil))[:8]
}
