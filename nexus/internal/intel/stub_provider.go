package intel

import (
	"context"
	"fmt"
	"sync"
)

// StubProvider is a deterministic LLM provider for testing.
// It returns pre-loaded responses in order, cycling back to the start
// when all responses have been consumed.
type StubProvider struct {
	mu        sync.Mutex
	name      string
	responses []QueryResponse // pre-loaded responses, returned in order
	idx       int
	healthy   bool
	calls     int // total number of Query() calls
}

// NewStubProvider creates a new StubProvider with the given name and pre-loaded responses.
func NewStubProvider(name string, responses []QueryResponse) *StubProvider {
	return &StubProvider{
		name:      name,
		responses: responses,
		healthy:   true,
	}
}

// Name returns the provider's identifier.
func (s *StubProvider) Name() string {
	return s.name
}

// Query returns the next pre-loaded response. If the provider is unhealthy
// it returns an error. If all responses have been consumed, it cycles.
func (s *StubProvider) Query(_ context.Context, _ QueryRequest) (*QueryResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.calls++

	if !s.healthy {
		return nil, fmt.Errorf("provider %s is unhealthy", s.name)
	}

	if len(s.responses) == 0 {
		return nil, fmt.Errorf("provider %s has no responses configured", s.name)
	}

	resp := s.responses[s.idx]
	resp.Provider = s.name
	s.idx = (s.idx + 1) % len(s.responses)
	return &resp, nil
}

// Health returns the current health status of the stub provider.
func (s *StubProvider) Health() ProviderHealth {
	s.mu.Lock()
	defer s.mu.Unlock()

	h := ProviderHealth{
		Available:    s.healthy,
		ErrorRate:    0.0,
		LatencyP95Ms: 50,
	}
	if !s.healthy {
		h.ErrorRate = 1.0
		h.LastError = "provider marked unhealthy"
	}
	return h
}

// SetHealthy sets whether the stub provider is healthy.
func (s *StubProvider) SetHealthy(healthy bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.healthy = healthy
}

// Calls returns the total number of Query() invocations.
func (s *StubProvider) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}
