package temporal

import "context"

// Interface is the subset of Client used by the controller.
// Declared here so tests can supply a mock without hitting the real API.
type Interface interface {
	GetNamespaceInfo(ctx context.Context, namespace string) (*NamespaceInfo, error)
	GetCurrentAPS(ctx context.Context, namespace string) (float64, error)
	SetTRU(ctx context.Context, namespace string, newTRU int) error
}
