// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A member that opens the stream and sends nothing is not answering: the caller has to be told
// that, so the router falls over to the next member instead of reporting a stopped run.
func TestASilentStreamIsAFailureWithAReason(t *testing.T) {
	restore := SetStreamLimitsForTest(200*time.Millisecond, 400*time.Millisecond, 20*time.Millisecond)
	t.Cleanup(restore)

	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		time.Sleep(time.Second) // never a piece
	}))
	defer silent.Close()

	_, err := DoChatStream(context.Background(), silent.Client(), silent.URL, nil,
		ChatRequest{Model: "quiet-model"}, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrStreamSilent{})
	assert.Contains(t, err.Error(), "quiet-model", "the reason names the model that stayed silent")

	var reason ErrStreamSilent
	require.ErrorAs(t, err, &reason)
	assert.True(t, reason.First, "nothing arrived at all")
}

// A gateway may front several kinds of model and list each kind apart: the endpoint a model serves
// is what the listing is filtered by.
func TestListModelsServingAsksForTheEndpoint(t *testing.T) {
	asked := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"parallel/decisions-fast:free","name":"Parallel: Decisions Fast (free)"}]}`))
	}))
	defer server.Close()

	models, err := DoListModelsServing(context.Background(), server.Client(), server.URL+"/models", nil, "/api/alpha/decisions")
	require.NoError(t, err)
	require.Len(t, models, 1)
	assert.Equal(t, "parallel/decisions-fast:free", models[0].ID)
	assert.Equal(t, "endpoints=%2Fapi%2Falpha%2Fdecisions", asked)
}
