package protocol

// RepoID identifies a Dolt repository for synchronization purposes.
//
// It is intentionally small and transport-agnostic (no peer addressing, no URLs).
type RepoID struct {
	Org      string
	RepoName string
}

func (r RepoID) IsZero() bool {
	return r.Org == "" && r.RepoName == ""
}

func (r RepoID) Equal(other RepoID) bool {
	return r.Org == other.Org && r.RepoName == other.RepoName
}
