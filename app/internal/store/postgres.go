package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"resilience-stack/app/internal/domain"
)

//go:embed migrations/*.sql
var migrations embed.FS

// migrationLockID serialises migrations when blue and green start together.
const migrationLockID = 727274

const uniqueViolation = "23505"

// Postgres implements Store on top of a pgx connection pool.
type Postgres struct {
	pool *pgxpool.Pool
}

// Open connects to Postgres, waiting up to the context deadline for the
// database to accept connections, and applies pending migrations.
func Open(ctx context.Context, dsn string) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.MaxConns = 10
	cfg.MaxConnIdleTime = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	for {
		err := pool.Ping(ctx)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, fmt.Errorf("database not reachable: %w", err)
		case <-time.After(500 * time.Millisecond):
		}
	}

	p := &Postgres{pool: pool}
	if err := p.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return p, nil
}

// Close releases all connections.
func (p *Postgres) Close() { p.pool.Close() }

func (p *Postgres) migrate(ctx context.Context) error {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return fmt.Errorf("take migration lock: %w", err)
	}
	defer func() {
		// Use a fresh context: the lock must be freed even if ctx has expired.
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", migrationLockID)
	}()

	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	sort.Strings(names)
	for _, name := range names {
		sql, err := migrations.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
	}
	return nil
}

// Ping reports whether the database answers.
func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

// Tariffs lists all tariffs.
func (p *Postgres) Tariffs(ctx context.Context) ([]domain.Tariff, error) {
	rows, err := p.pool.Query(ctx, "SELECT id, name, cost FROM tariff ORDER BY id")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Tariff, error) {
		var t domain.Tariff
		err := row.Scan(&t.ID, &t.Name, &t.CostPerHrs)
		return t, err
	})
}

// Spots lists parking spots, optionally only free ones or one type.
func (p *Postgres) Spots(ctx context.Context, filter SpotFilter) ([]domain.Spot, error) {
	const query = `
		SELECT ps.id, ps.spot_type, ps.is_reserved, t.id, t.name, t.cost
		FROM parking_spot ps
		JOIN tariff t ON t.id = ps.tariff_id
		WHERE ($1 = FALSE OR ps.is_reserved = FALSE)
		  AND ($2 = '' OR ps.spot_type = $2)
		ORDER BY ps.id`
	rows, err := p.pool.Query(ctx, query, filter.OnlyFree, string(filter.Type))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Spot, error) {
		var s domain.Spot
		err := row.Scan(&s.ID, &s.Type, &s.Reserved, &s.Tariff.ID, &s.Tariff.Name, &s.Tariff.CostPerHrs)
		return s, err
	})
}

// Reserve atomically takes a free spot for a vehicle, registering the vehicle
// on its first visit.
func (p *Postgres) Reserve(ctx context.Context, spotID int, plate string) (domain.Reservation, error) {
	res := domain.Reservation{SpotID: spotID, LicensePlate: plate}
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var reserved bool
		err := tx.QueryRow(ctx, "SELECT is_reserved FROM parking_spot WHERE id = $1 FOR UPDATE", spotID).Scan(&reserved)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrSpotNotFound
		}
		if err != nil {
			return err
		}
		if reserved {
			return domain.ErrSpotTaken
		}

		var vehicleID int
		err = tx.QueryRow(ctx, `
			INSERT INTO vehicles (license_plate) VALUES ($1)
			ON CONFLICT (license_plate) DO UPDATE SET license_plate = EXCLUDED.license_plate
			RETURNING id`, plate).Scan(&vehicleID)
		if err != nil {
			return err
		}

		err = tx.QueryRow(ctx, `
			INSERT INTO reservation (vehicle_id, parking_spot_id) VALUES ($1, $2)
			RETURNING id, start_time`, vehicleID, spotID).Scan(&res.ID, &res.StartedAt)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
				return domain.ErrVehicleParked
			}
			return err
		}

		_, err = tx.Exec(ctx, "UPDATE parking_spot SET is_reserved = TRUE WHERE id = $1", spotID)
		return err
	})
	return res, err
}

// Reservation loads one reservation.
func (p *Postgres) Reservation(ctx context.Context, id int) (domain.Reservation, error) {
	var res domain.Reservation
	err := p.pool.QueryRow(ctx, `
		SELECT r.id, r.parking_spot_id, v.license_plate, r.start_time, r.end_time
		FROM reservation r
		JOIN vehicles v ON v.id = r.vehicle_id
		WHERE r.id = $1`, id).Scan(&res.ID, &res.SpotID, &res.LicensePlate, &res.StartedAt, &res.EndedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return res, domain.ErrReservationAbsent
	}
	return res, err
}

// Release closes a reservation, frees its spot and issues the invoice.
func (p *Postgres) Release(ctx context.Context, id int) (domain.Invoice, error) {
	var invoice domain.Invoice
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var (
			spotID, cost int
			plate        string
			start        time.Time
			end          *time.Time
		)
		err := tx.QueryRow(ctx, `
			SELECT r.parking_spot_id, v.license_plate, r.start_time, r.end_time, t.cost
			FROM reservation r
			JOIN vehicles v ON v.id = r.vehicle_id
			JOIN parking_spot ps ON ps.id = r.parking_spot_id
			JOIN tariff t ON t.id = ps.tariff_id
			WHERE r.id = $1
			FOR UPDATE OF r`, id).Scan(&spotID, &plate, &start, &end, &cost)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrReservationAbsent
		}
		if err != nil {
			return err
		}
		if end != nil {
			return domain.ErrAlreadyClosed
		}

		var finished time.Time
		err = tx.QueryRow(ctx, "UPDATE reservation SET end_time = now() WHERE id = $1 RETURNING end_time", id).Scan(&finished)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE parking_spot SET is_reserved = FALSE WHERE id = $1", spotID); err != nil {
			return err
		}
		invoice = domain.NewInvoice(id, plate, finished.Sub(start), cost)
		return nil
	})
	return invoice, err
}
