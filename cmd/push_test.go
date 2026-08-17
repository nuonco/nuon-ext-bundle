package cmd

import (
	"testing"

	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/require"

	"github.com/nuonco/nuon/pkg/runner/oci/bundle"
)

func TestMakePlan(t *testing.T) {
	dgst := digest.FromString("artifact")
	items := makePlan([]bundle.Member{{Key: "action:a/step-artifact:s", Kind: "action-artifact", Name: "A Name/S!", Digest: dgst, Size: 42}}, "Customer Prefix")
	require.Len(t, items, 1)
	require.Equal(t, "customer-prefix/actions/a-name/s", items[0].Repo)
	require.Equal(t, "sha256-"+dgst.Encoded(), items[0].Tag)
}

func TestRegionFromRegistry(t *testing.T) {
	require.Equal(t, "us-west-2", regionFromRegistry("123456789012.dkr.ecr.us-west-2.amazonaws.com"))
}
