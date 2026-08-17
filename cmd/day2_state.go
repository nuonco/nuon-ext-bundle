package cmd

import (
	"context"

	"github.com/nuonco/nuon/pkg/runner/airgap/day2state"
)

var errStateObjectExists = day2state.ErrObjectExists

type day2State = day2state.State

func makeDay2State(ctx context.Context, state, profile, region string) (day2State, error) {
	return day2state.New(ctx, state, profile, region)
}
