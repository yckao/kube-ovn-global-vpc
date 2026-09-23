package v1

// DestinationRoutesStatus acknowledges native NB configuration, not BFD health
// or successful tenant packet delivery.
type DestinationRoutesStatus struct {
	Capability         string `json:"capability,omitempty"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	AppliedHash        string `json:"appliedHash,omitempty"`
	Ready              bool   `json:"ready"`
}
