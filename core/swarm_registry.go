package core

import (
	"sync"
)

var swarmProvidersMu sync.RWMutex
var swarmProviders = map[RepoID]ProviderPicker{}

// RegisterSwarmProviders registers a ProviderPicker used by the `swarm://` dbfactory.
//
// This is process-global because Dolt's dbfactory is process-global. Integrations typically
// run one Node per process, so this maps cleanly. If multiple repos exist in one process,
// register each RepoID separately.
func RegisterSwarmProviders(repo RepoID, picker ProviderPicker) {
	if picker == nil {
		return
	}
	swarmProvidersMu.Lock()
	defer swarmProvidersMu.Unlock()
	swarmProviders[repo] = picker
}

func UnregisterSwarmProviders(repo RepoID) {
	swarmProvidersMu.Lock()
	defer swarmProvidersMu.Unlock()
	delete(swarmProviders, repo)
}

func getSwarmProviderPicker(repo RepoID) (ProviderPicker, bool) {
	swarmProvidersMu.RLock()
	defer swarmProvidersMu.RUnlock()
	p, ok := swarmProviders[repo]
	return p, ok
}
