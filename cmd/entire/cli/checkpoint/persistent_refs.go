package checkpoint

import (
	"context"
	"slices"

	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// PersistentRefs is the committed-metadata ref topology.
type PersistentRefs struct {
	Primary plumbing.ReferenceName
	Read    plumbing.ReferenceName
	Push    []plumbing.ReferenceName
}

// DefaultV1Refs returns the v1-only topology.
func DefaultV1Refs() PersistentRefs {
	v1Branch := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	return PersistentRefs{
		Primary: v1Branch,
		Read:    v1Branch,
		Push:    []plumbing.ReferenceName{v1Branch},
	}
}

// PrimaryFetchableFromRemote reports whether Primary has a remote-tracking
// shadow, for a given remote.
func (r PersistentRefs) PrimaryFetchableFromRemote() bool {
	return r.Primary.IsBranch() && slices.Contains(r.Push, r.Primary)
}

// ReadBootstrappableFromRemote reports whether reads can be bootstrapped from
// a given remote: true when reads target Primary and Primary is fetchable
// from that remote.
func (r PersistentRefs) ReadBootstrappableFromRemote() bool {
	return r.Read == r.Primary && r.PrimaryFetchableFromRemote()
}

// PrimaryAsRead returns a copy of r with Read pinned to Primary.
func (r PersistentRefs) PrimaryAsRead() PersistentRefs {
	r.Read = r.Primary
	return r
}

// ResolveRefs returns the committed metadata topology.
func ResolveRefs(_ context.Context) PersistentRefs {
	return DefaultV1Refs()
}
