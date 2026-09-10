// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package client

import (
	"context"
	"errors"
	"testing"

	pbgo "github.com/DataDog/datadog-agent/pkg/proto/pbgo/core"
	"github.com/stretchr/testify/require"
)

type emptyConfigFetcher struct{}

func (emptyConfigFetcher) ClientGetConfigs(context.Context, *pbgo.ClientGetConfigsRequest) (*pbgo.ClientGetConfigsResponse, error) {
	return &pbgo.ClientGetConfigsResponse{}, nil
}

type failingConfigFetcher struct{}

func (failingConfigFetcher) ClientGetConfigs(context.Context, *pbgo.ClientGetConfigsRequest) (*pbgo.ClientGetConfigsResponse, error) {
	return nil, errors.New("remote config unavailable")
}

func TestInitialUpdateDoneClosesForAuthoritativeEmptySnapshot(t *testing.T) {
	client, err := newClient(emptyConfigFetcher{}, WithoutTufVerification())
	require.NoError(t, err)

	select {
	case <-client.InitialUpdateDone():
		t.Fatal("initial update was marked complete before the first fetch")
	default:
	}

	require.NoError(t, client.update())
	select {
	case <-client.InitialUpdateDone():
	default:
		t.Fatal("initial update was not marked complete after an empty successful fetch")
	}
}

func TestInitialUpdateDoneRemainsOpenAfterFailedFetch(t *testing.T) {
	client, err := newClient(failingConfigFetcher{}, WithoutTufVerification())
	require.NoError(t, err)
	require.Error(t, client.update())

	select {
	case <-client.InitialUpdateDone():
		t.Fatal("failed fetch was treated as an authoritative snapshot")
	default:
	}
}
