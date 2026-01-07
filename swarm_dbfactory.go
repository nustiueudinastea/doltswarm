package doltswarm

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	"github.com/dolthub/dolt/go/store/types"
)

const SwarmDBFactoryScheme = "swarm"

func init() {
	dbfactory.RegisterFactory(SwarmDBFactoryScheme, SwarmDBFactory{})
}

// SwarmDBFactory is a read-only dbfactory scheme used to let Dolt execute optimized fetches
// against peers in a provider-agnostic way.
//
// URL parsing rules:
// - swarm://<repo>               => RepoID{RepoName:<repo>}
// - swarm://<org>/<repo>         => RepoID{Org:<org>, RepoName:<repo>}
// - swarm:///repo                => RepoID{RepoName:<repo>}
type SwarmDBFactory struct{}

func (SwarmDBFactory) PrepareDB(ctx context.Context, nbf *types.NomsBinFormat, urlObj *url.URL, params map[string]interface{}) error {
	return fmt.Errorf("swarm dbfactory is read-only")
}

func (SwarmDBFactory) CreateDB(ctx context.Context, nbf *types.NomsBinFormat, urlObj *url.URL, params map[string]interface{}) (datas.Database, types.ValueReadWriter, tree.NodeStore, error) {
	repo := parseSwarmRepoID(urlObj)
	if repo.RepoName == "" {
		return nil, nil, nil, fmt.Errorf("swarm url missing repo name: %q", urlObj.String())
	}

	picker, ok := getSwarmProviderPicker(repo)
	if !ok {
		return nil, nil, nil, fmt.Errorf("no swarm ProviderPicker registered for repo %q", repo.RepoName)
	}

	provider, err := picker.PickProvider(ctx, repo)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("pick provider: %w", err)
	}

	cs, err := NewRemoteChunkStore(provider, repo, nbf.VersionString())
	if err != nil {
		return nil, nil, nil, err
	}

	vrw := types.NewValueStore(cs)
	ns := tree.NewNodeStore(cs)
	typesDB := datas.NewTypesDatabase(vrw, ns)

	return typesDB, vrw, ns, nil
}

func parseSwarmRepoID(urlObj *url.URL) RepoID {
	path := strings.Trim(urlObj.Path, "/")
	if urlObj.Host != "" && path != "" {
		return RepoID{Org: urlObj.Host, RepoName: path}
	}
	if path != "" {
		return RepoID{RepoName: path}
	}
	return RepoID{RepoName: urlObj.Host}
}
