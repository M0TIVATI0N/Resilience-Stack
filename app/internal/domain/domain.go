// Package domain holds the parking business rules, ported from the original
// Smart Parking Management System (Java) to Go.
package domain

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

// Business errors. The HTTP layer maps each of them to a 4xx status, so they
// never count against the release's 5xx error budget.
var (
	ErrInvalidPlate      = errors.New("invalid license plate")
	ErrSpotNotFound      = errors.New("parking spot not found")
	ErrSpotTaken         = errors.New("this spot is already taken")
	ErrVehicleParked     = errors.New("this vehicle already has an active reservation")
	ErrReservationAbsent = errors.New("reservation not found")
	ErrAlreadyClosed     = errors.New("this reservation has already been closed")
)

// SpotType is the kind of parking spot.
type SpotType string

// Supported spot types.
const (
	SpotStandard SpotType = "standard"
	SpotElectric SpotType = "electric"
	SpotDisabled SpotType = "disabled"
)

// ParseSpotType validates a spot type coming from a client.
func ParseSpotType(s string) (SpotType, bool) {
	switch t := SpotType(strings.ToLower(strings.TrimSpace(s))); t {
	case SpotStandard, SpotElectric, SpotDisabled:
		return t, true
	default:
		return "", false
	}
}

// Tariff is an hourly price in KZT.
type Tariff struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	CostPerHrs int    `json:"cost_per_hour_kzt"`
}

// Spot is a single parking place.
type Spot struct {
	ID       int      `json:"id"`
	Type     SpotType `json:"type"`
	Reserved bool     `json:"reserved"`
	Tariff   Tariff   `json:"tariff"`
}

// Reservation binds a vehicle to a spot for a period of time.
type Reservation struct {
	ID           int        `json:"id"`
	SpotID       int        `json:"spot_id"`
	LicensePlate string     `json:"license_plate"`
	StartedAt    time.Time  `json:"started_at"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
}

// Invoice is the receipt issued when a reservation is closed.
type Invoice struct {
	ReservationID int    `json:"reservation_id"`
	LicensePlate  string `json:"license_plate"`
	Duration      string `json:"duration"`
	BilledHours   int    `json:"billed_hours"`
	TotalKZT      int    `json:"total_kzt"`
}

var platePattern = regexp.MustCompile(`^[A-Z0-9][A-Z0-9 -]{0,14}[A-Z0-9]$`)

// NormalizePlate upper-cases and validates a license plate.
func NormalizePlate(raw string) (string, error) {
	plate := strings.ToUpper(strings.TrimSpace(raw))
	if !platePattern.MatchString(plate) {
		return "", fmt.Errorf("%w: expected 2-16 letters, digits, spaces or dashes", ErrInvalidPlate)
	}
	return plate, nil
}

// BilledHours rounds a stay up to whole hours, with a one-hour minimum.
func BilledHours(stay time.Duration) int {
	hours := int(math.Ceil(stay.Hours()))
	if hours < 1 {
		return 1
	}
	return hours
}

// NewInvoice builds the receipt for a finished stay.
func NewInvoice(reservationID int, plate string, stay time.Duration, costPerHour int) Invoice {
	hours := BilledHours(stay)
	return Invoice{
		ReservationID: reservationID,
		LicensePlate:  plate,
		Duration:      stay.Round(time.Second).String(),
		BilledHours:   hours,
		TotalKZT:      hours * costPerHour,
	}
}
