package checks

import (
	"fmt"

	"github.com/cosmosdevops/kube-refs-mcp/internal/links"
)

// Explanation is the detail for one link.
type Explanation struct {
	Finding links.Link `json:"finding"`
	// Summary says in one line what is wrong, or that nothing is.
	Summary string `json:"summary"`
	// Meaning says what the status claims and what it does not.
	Meaning  string `json:"meaning"`
	Expected string `json:"expected,omitempty"`
	Found    string `json:"found,omitempty"`
	// Candidates lists what does exist where the target was looked for.
	Candidates []string `json:"candidates,omitempty"`
	Fix        string   `json:"fix,omitempty"`
}

var meanings = map[links.Status]string{
	links.StatusOK:         "Both ends are known and they fit.",
	links.StatusBroken:     "Certain. Both ends are known and they do not fit, so this will not work when applied.",
	links.StatusUnresolved: "Not decided. This is not a pass and not a failure.",
	links.StatusSkipped:    "Not checked, because the check does not apply here. It does not count for or against the bundle.",
	links.StatusError:      "The check failed to run. Treat the link as not checked.",
}

var reasonMeanings = map[links.Reason]string{
	links.ReasonNotInBundle:      "The target is not in the bundle. It may already exist in the cluster. Add it, pass it in clusterObjects, or set bundleComplete if nothing else exists.",
	links.ReasonNamespaceUnknown: "One of the two resources sets no namespace, so it is not known whether they are in the same one. Pass the namespace the bundle is deployed to.",
	links.ReasonPortNotDeclared:  "The pods do not declare this port, but that is not certain to be a mistake. A container does not have to declare a port number it listens on. A named port may be declared by a container the cluster adds to the pod, or be meant for other pods the Service selects. Set bundleComplete if nothing else will exist.",
	links.ReasonInvalid:          "The reference is not well-formed, so what it points at is not known. Correct it and check again.",
	links.ReasonDetailsUnknown:   "The target is known, but not the part of it this reference needs.",
}

// Explain returns the detail for one link of the result.
func (r *Result) Explain(id string) (*Explanation, error) {
	for _, l := range r.Links {
		if l.ID != id {
			continue
		}
		ex := &Explanation{Finding: l, Summary: l.Message, Meaning: meanings[l.Status], Expected: l.Expected, Found: l.Found, Candidates: l.Candidates, Fix: l.Fix}
		if l.Status == links.StatusUnresolved {
			if m := reasonMeanings[l.Reason]; m != "" {
				ex.Meaning += " " + m
			}
		}
		if l.Status == links.StatusBroken && l.Reason == links.ReasonNotInBundle {
			ex.Meaning = "The target is not in the bundle, and the bundle was said to be complete. Without that option this would be unresolved."
		}
		return ex, nil
	}
	return nil, fmt.Errorf("finding %s is not part of check %s", id, r.Report.CheckID)
}
