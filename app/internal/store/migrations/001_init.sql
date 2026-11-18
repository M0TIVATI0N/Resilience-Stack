-- Schema changes must stay backward compatible with the previous release:
-- blue and green run side by side, and a rollback puts old code on this schema.

CREATE TABLE IF NOT EXISTS tariff (
    id   SERIAL PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    cost INTEGER NOT NULL CHECK (cost >= 0)
);

CREATE TABLE IF NOT EXISTS parking_spot (
    id          SERIAL PRIMARY KEY,
    spot_type   TEXT NOT NULL DEFAULT 'standard'
                CHECK (spot_type IN ('standard', 'electric', 'disabled')),
    tariff_id   INTEGER NOT NULL REFERENCES tariff (id),
    is_reserved BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE TABLE IF NOT EXISTS vehicles (
    id            SERIAL PRIMARY KEY,
    license_plate TEXT NOT NULL UNIQUE
);

CREATE TABLE IF NOT EXISTS reservation (
    id              SERIAL PRIMARY KEY,
    vehicle_id      INTEGER NOT NULL REFERENCES vehicles (id),
    parking_spot_id INTEGER NOT NULL REFERENCES parking_spot (id),
    start_time      TIMESTAMPTZ NOT NULL DEFAULT now(),
    end_time        TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS reservation_active_spot
    ON reservation (parking_spot_id) WHERE end_time IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS reservation_active_vehicle
    ON reservation (vehicle_id) WHERE end_time IS NULL;

INSERT INTO tariff (name, cost) VALUES
    ('standard', 500),
    ('electric', 800),
    ('disabled', 200)
ON CONFLICT (name) DO NOTHING;

INSERT INTO parking_spot (spot_type, tariff_id)
SELECT seed.spot_type, t.id
FROM (
    SELECT 'standard' AS spot_type, generate_series(1, 300)
    UNION ALL SELECT 'electric', generate_series(1, 60)
    UNION ALL SELECT 'disabled', generate_series(1, 40)
) AS seed
JOIN tariff t ON t.name = seed.spot_type
WHERE NOT EXISTS (SELECT 1 FROM parking_spot);
