-- +goose Up
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE orders (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_no text NOT NULL UNIQUE CHECK (order_no ~ '^ORD-[0-9]{13}-[0-9]{3}$'),
    idempotency_key text NOT NULL UNIQUE,
    request_hash text NOT NULL,
    customer_ref text NOT NULL,
    product_code text NOT NULL,
    player_uid text NOT NULL,
    amount numeric(18,2) NOT NULL CHECK (amount > 0),
    status text NOT NULL CHECK (status IN ('PENDING_PAYMENT','QUEUED','IN_PROGRESS','COMPLETED','DELAYED','FAILED','EXPIRED','REFUNDED')),
    retry_count integer NOT NULL DEFAULT 0 CHECK (retry_count >= 0),
    next_retry_at timestamptz,
    lease_owner text,
    lease_expires_at timestamptz,
    last_heartbeat_at timestamptz,
    expires_at timestamptz NOT NULL,
    completed_at timestamptz,
    version integer NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX orders_current_idx ON orders(status, next_retry_at, created_at);
CREATE INDEX orders_created_idx ON orders(created_at DESC);
CREATE INDEX orders_lease_idx ON orders(lease_expires_at) WHERE status = 'IN_PROGRESS';

CREATE TABLE order_timeline (
    id bigserial PRIMARY KEY,
    order_id uuid NOT NULL REFERENCES orders(id) ON DELETE RESTRICT,
    event_type text NOT NULL,
    from_status text,
    to_status text,
    message text NOT NULL,
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX order_timeline_order_idx ON order_timeline(order_id, id);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION reject_timeline_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'order_timeline is append-only';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
CREATE TRIGGER order_timeline_no_update BEFORE UPDATE OR DELETE ON order_timeline
FOR EACH ROW EXECUTE FUNCTION reject_timeline_mutation();

CREATE TABLE issue_reports (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id uuid NOT NULL REFERENCES orders(id) ON DELETE RESTRICT,
    issue_type text NOT NULL,
    description text NOT NULL,
    status text NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN','RESOLVED')),
    created_at timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz
);

CREATE TABLE payment_attempts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id uuid NOT NULL REFERENCES orders(id) ON DELETE RESTRICT,
    attempt_no integer NOT NULL,
    provider_ref text UNIQUE,
    status text NOT NULL CHECK (status IN ('STARTED','SUCCEEDED','FAILED')),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(order_id, attempt_no)
);

CREATE TABLE webhook_events (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    source text NOT NULL,
    event_id text NOT NULL,
    order_no text NOT NULL,
    payload jsonb NOT NULL,
    result text NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(source, event_id)
);

CREATE TABLE order_alerts (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id uuid NOT NULL REFERENCES orders(id) ON DELETE RESTRICT,
    alert_type text NOT NULL,
    message text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz
);
CREATE UNIQUE INDEX order_alerts_active_unique ON order_alerts(order_id, alert_type) WHERE resolved_at IS NULL;

-- +goose Down
DROP TABLE IF EXISTS order_alerts;
DROP TABLE IF EXISTS webhook_events;
DROP TABLE IF EXISTS payment_attempts;
DROP TABLE IF EXISTS issue_reports;
DROP TRIGGER IF EXISTS order_timeline_no_update ON order_timeline;
DROP FUNCTION IF EXISTS reject_timeline_mutation;
DROP TABLE IF EXISTS order_timeline;
DROP TABLE IF EXISTS orders;
