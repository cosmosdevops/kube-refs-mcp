// Package links defines the result model shared by the checks, the CLI and
// the MCP server. A link is one reference from a resource to something it
// needs: a Service to the pods it selects, a pod to a Secret, and so on.
package links

import "sort"

// Status is the outcome of checking one link.
type Status string

const (
	// StatusOK: the target is known and matches.
	StatusOK Status = "ok"
	// StatusBroken: the link certainly does not work. Both ends are known
	// and they do not fit.
	StatusBroken Status = "broken"
	// StatusUnresolved: the link could not be decided, most often because
	// the target is not in the bundle and may exist in the cluster. This is
	// never reported as a pass.
	StatusUnresolved Status = "unresolved"
	// StatusSkipped: the check does not apply, or the kind is not understood.
	StatusSkipped Status = "skipped"
	// StatusError: the check itself failed. Never treated as a pass.
	StatusError Status = "error"
)

// AllStatuses lists every status in display order.
var AllStatuses = []Status{StatusBroken, StatusError, StatusUnresolved, StatusOK, StatusSkipped}

// Actionable reports whether a link needs the caller's attention.
func (s Status) Actionable() bool {
	return s == StatusBroken || s == StatusUnresolved || s == StatusError
}

// Reason says why a link has its status, in a form a program can test.
type Reason string

const (
	// ReasonNotInBundle: nothing in the bundle or the supplied cluster
	// objects is the target. With bundleComplete this becomes broken.
	ReasonNotInBundle Reason = "not_in_bundle"
	// ReasonNamespaceUnknown: one end has no namespace, so the two cannot
	// be compared. Pass a namespace to settle it.
	ReasonNamespaceUnknown Reason = "namespace_unknown"
	// ReasonPortNotDeclared: the pods do not list the port, and that is not
	// certain to be a mistake. Kubernetes does not require a numeric port to
	// be listed, and a named port may be meant for other pods.
	ReasonPortNotDeclared Reason = "port_not_declared"
	// ReasonDetailsUnknown: the target is known, but not the part of it
	// the reference needs: its keys or its ports. Something else may fill
	// them in.
	ReasonDetailsUnknown Reason = "details_unknown"
	// ReasonKeyMissing: the ConfigMap or Secret does not have the key.
	ReasonKeyMissing Reason = "key_missing"
	// ReasonPortMissing: the target does not have the port.
	ReasonPortMissing Reason = "port_missing"
	// ReasonSelectorMismatch: a workload's selector does not match its own
	// pod template.
	ReasonSelectorMismatch Reason = "selector_mismatch"
	// ReasonWrongKind: the reference names a kind that cannot be used there.
	ReasonWrongKind Reason = "wrong_kind"
	// ReasonOptional: the reference is marked optional and the target, or
	// its key, is absent.
	ReasonOptional Reason = "optional"
	// ReasonClusterProvided: the cluster creates the target itself.
	ReasonClusterProvided Reason = "cluster_provided"
	// ReasonRuntimeLabels: the selector uses a label the cluster only adds
	// to running pods.
	ReasonRuntimeLabels Reason = "runtime_labels"
	// ReasonNotApplicable: there is nothing to check for this resource.
	ReasonNotApplicable Reason = "not_applicable"
	// ReasonInvalid: the reference is not well-formed, so it was not
	// followed. Schema validation reports the detail.
	ReasonInvalid Reason = "invalid"
	// ReasonCheckFailed: the check stopped with an internal error.
	ReasonCheckFailed Reason = "check_failed"
)

// Ref identifies a Kubernetes resource in a request.
type Ref struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
	// Source is the file and document index the resource came from, or
	// "clusterObjects" for an object supplied as context.
	Source string `json:"source,omitempty"`
}

// Key returns a stable identity string for the resource.
func (r Ref) Key() string {
	return r.Source + "|" + r.APIVersion + "|" + r.Kind + "|" + r.Namespace + "|" + r.Name
}

// String renders Kind/namespace/name.
func (r Ref) String() string {
	if r.Namespace != "" {
		return r.Kind + "/" + r.Namespace + "/" + r.Name
	}
	return r.Kind + "/" + r.Name
}

// Target is the far end of a link: what the reference points at.
type Target struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	// Name is empty for a selector that matched nothing.
	Name string `json:"name,omitempty"`
	// Selector is the label selector, for links made by labels.
	Selector string `json:"selector,omitempty"`
	// Key is the ConfigMap or Secret key referred to, if any.
	Key string `json:"key,omitempty"`
	// Port is the port name or number referred to, if any.
	Port string `json:"port,omitempty"`
	// Source is set when the target was found: where it came from.
	Source string `json:"source,omitempty"`
}

// String renders the target for messages.
func (t Target) String() string {
	s := t.Kind
	if t.Name != "" {
		if t.Namespace != "" {
			s += "/" + t.Namespace
		}
		s += "/" + t.Name
	} else if t.Selector != "" {
		s += " with labels " + t.Selector
	}
	if t.Key != "" {
		s += " key " + t.Key
	}
	if t.Port != "" {
		s += " port " + t.Port
	}
	return s
}

// Link is one checked reference. A link whose status needs attention is
// also called a finding.
type Link struct {
	ID string `json:"id"`
	// Check names the check that produced the link, for example
	// "service-selector".
	Check  string `json:"check"`
	Status Status `json:"status"`
	Reason Reason `json:"reason,omitempty"`
	From   Ref    `json:"from"`
	// Field is the path in the From resource that holds the reference.
	Field   string `json:"field,omitempty"`
	To      Target `json:"to"`
	Message string `json:"message"`
	// ClosestMatch names the nearest thing that does exist, when there is
	// one. It is a hint, not a certainty.
	ClosestMatch string `json:"closestMatch,omitempty"`
	Fix          string `json:"fix,omitempty"`

	// Expected, Found and Candidates feed explain_finding. They are not
	// part of the listing.
	Expected   string   `json:"-"`
	Found      string   `json:"-"`
	Candidates []string `json:"-"`
}

// Summary counts the links in a report.
type Summary struct {
	Resources  int `json:"resources"`
	Links      int `json:"links"`
	OK         int `json:"ok"`
	Broken     int `json:"broken"`
	Unresolved int `json:"unresolved"`
	Skipped    int `json:"skipped"`
	Errors     int `json:"errors"`
}

// SkippedFile is a file that was found but not checked.
type SkippedFile struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Verdict is the overall result of a report.
type Verdict string

const (
	// VerdictPass: every link is ok or skipped.
	VerdictPass Verdict = "pass"
	// VerdictFail: at least one link is broken.
	VerdictFail Verdict = "fail"
	// VerdictUnknown: nothing is broken, but some links are unresolved or
	// errored.
	VerdictUnknown Verdict = "unknown"
)

// Report is the result of one check request.
type Report struct {
	CheckID string `json:"checkId"`
	// Status is "fail" if any link is broken, otherwise "unknown" if the
	// report is incomplete, otherwise "pass".
	Status Verdict `json:"status"`
	// Complete is false when any link is unresolved or errored. It is
	// reported separately from the verdict.
	Complete bool `json:"complete"`
	// Consistent is true only when nothing is broken and the report is
	// complete.
	Consistent bool `json:"consistent"`
	// BundleComplete repeats the caller's option: the bundle was said to
	// hold every object, so a missing target counted as broken.
	BundleComplete bool `json:"bundleComplete"`
	// ChecksRun and ChecksSkipped name the checks that were on and off.
	// The verdict covers only the ones that ran.
	ChecksRun     []string `json:"checksRun"`
	ChecksSkipped []string `json:"checksSkipped,omitempty"`
	Summary       Summary  `json:"summary"`
	Resources     []Ref    `json:"resources"`
	// Findings holds the links that need attention (broken, unresolved,
	// error). Links that are ok or skipped are listed only when requested.
	Findings     []Link        `json:"findings"`
	SkippedFiles []SkippedFile `json:"skippedFiles,omitempty"`
	// Notes lists assumptions and limits that applied to this check.
	Notes []string `json:"notes,omitempty"`
}

// Aggregate fills Status, Complete, Consistent and the Summary counts from
// every link, including the ones that are ok or skipped.
func Aggregate(r *Report, all []Link) {
	s := &r.Summary
	s.Links = len(all)
	for _, l := range all {
		switch l.Status {
		case StatusOK:
			s.OK++
		case StatusBroken:
			s.Broken++
		case StatusUnresolved:
			s.Unresolved++
		case StatusSkipped:
			s.Skipped++
		case StatusError:
			s.Errors++
		}
	}
	r.Complete = s.Unresolved == 0 && s.Errors == 0
	switch {
	case s.Broken > 0:
		r.Status = VerdictFail
	case !r.Complete:
		r.Status = VerdictUnknown
	default:
		r.Status = VerdictPass
	}
	r.Consistent = s.Broken == 0 && r.Complete
}

// FileGroup groups findings by source file and resource.
type FileGroup struct {
	Path      string          `json:"path"`
	Resources []ResourceGroup `json:"resources"`
}

// ResourceGroup holds the findings for one resource.
type ResourceGroup struct {
	Resource Ref    `json:"resource"`
	Status   Status `json:"status"`
	Findings []Link `json:"findings"`
}

// GroupBySource arranges the report's findings by file and resource. Every
// checked resource appears, even when it has no listed findings.
func GroupBySource(r *Report, filePath func(Ref) string) []FileGroup {
	byRes := map[string][]Link{}
	for _, f := range r.Findings {
		byRes[f.From.Key()] = append(byRes[f.From.Key()], f)
	}
	files := map[string]*FileGroup{}
	var order []string
	for _, ref := range r.Resources {
		p := filePath(ref)
		g, ok := files[p]
		if !ok {
			g = &FileGroup{Path: p}
			files[p] = g
			order = append(order, p)
		}
		fs := byRes[ref.Key()]
		if fs == nil {
			fs = []Link{}
		}
		g.Resources = append(g.Resources, ResourceGroup{Resource: ref, Status: Worst(fs), Findings: fs})
	}
	sort.Strings(order)
	out := make([]FileGroup, 0, len(order))
	for _, p := range order {
		out = append(out, *files[p])
	}
	return out
}

// Worst returns the most serious status among the links, or ok for none.
func Worst(fs []Link) Status {
	rank := map[Status]int{StatusBroken: 4, StatusError: 3, StatusUnresolved: 2, StatusOK: 1, StatusSkipped: 0}
	worst := StatusOK
	for _, f := range fs {
		if rank[f.Status] > rank[worst] {
			worst = f.Status
		}
	}
	return worst
}
