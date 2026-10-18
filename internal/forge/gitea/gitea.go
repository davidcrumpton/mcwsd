package gitea

import (
	"context"
	"fmt"

	"github.com/davidcrumpton/mcwsd/internal/forge"
	forgejo "github.com/davidcrumpton/mcwsd/internal/forge/forgejo"
)

type Forge struct {
	*forgejo.Forge
}

func New(apiBase, token, projectPath string) *Forge {
	return &Forge{Forge: forgejo.New(apiBase, token, projectPath)}
}

var _ forge.Forge = (*Forge)(nil)

func (f *Forge) Name() string { return "gitea" }

func (f *Forge) OpenMergeRequest(ctx context.Context, req forge.OpenMRRequest) (*forge.OpenMRResult, error) {
	if f == nil || f.Forge == nil {
		return nil, fmt.Errorf("nil gitea forge")
	}
	return f.Forge.OpenMergeRequest(ctx, req)
}
