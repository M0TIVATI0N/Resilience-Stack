// Package store is the persistence layer of the parking service.
package store

import (
	"context"

	"resilience-stack/app/internal/domain"
)

// SpotFilter narrows a spot listing.
type SpotFilter struct {
	OnlyFree bool
	Type     domain.SpotType
}

// Store is everything the HTTP layer needs from persistence.
type Store interface {
	Ping(ctx context.Context) error
	Tariffs(ctx context.Context) ([]domain.Tariff, error)
	Spots(ctx context.Context, filter SpotFilter) ([]domain.Spot, error)
	Reserve(ctx context.Context, spotID int, plate string) (domain.Reservation, error)
	Reservation(ctx context.Context, id int) (domain.Reservation, error)
	Release(ctx context.Context, id int) (domain.Invoice, error)
}
