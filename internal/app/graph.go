package app

import (
	"context"

	"github.com/Nahua-Foundation/ragota/internal/graph"
)

// Graph passthroughs: thin delegations to the graph layer. The graph-expanded
// retrieval that composes them lives in context.go.

// GraphNeighbors returns edges in and out of a unit.
func (s *Service) GraphNeighbors(ctx context.Context, unitID string) (*graph.NeighborsResult, error) {
	return s.graph.Neighbors(ctx, unitID)
}

// GraphPath finds a directed path between two units.
func (s *Service) GraphPath(ctx context.Context, fromID, toID string, maxDepth int) ([]*graph.PathStep, error) {
	return s.graph.Path(ctx, fromID, toID, maxDepth)
}

// GraphTrace follows a parameter through calls and service contracts.
func (s *Service) GraphTrace(ctx context.Context, req *graph.TraceRequest) (*graph.TraceResult, error) {
	return s.graph.Trace(ctx, req)
}

// ServicesGraph lists detected services and aggregated inter-service links.
func (s *Service) ServicesGraph(ctx context.Context) ([]*graph.ServiceInfo, []*graph.ServiceLink, error) {
	return s.graph.ServicesGraph(ctx)
}

// Topics lists Kafka topics with their producers and consumers, optionally
// filtered to topics a service participates in.
func (s *Service) Topics(ctx context.Context, service string) ([]*graph.TopicInfo, error) {
	return s.graph.Topics(ctx, service)
}
