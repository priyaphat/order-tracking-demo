package order

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	db            *pgxpool.Pool
	leaseDuration time.Duration
	maxRetries    int
	queueSLA      time.Duration
}

type Order struct {
	OrderNo     string     `json:"orderNo"`
	CustomerRef string     `json:"customerRef"`
	ProductCode string     `json:"productCode"`
	PlayerUID   string     `json:"playerUid"`
	Amount      string     `json:"amount"`
	Status      Status     `json:"status"`
	RetryCount  int        `json:"retryCount"`
	NextRetryAt *time.Time `json:"nextRetryAt,omitempty"`
	ExpiresAt   time.Time  `json:"expiresAt"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
	Version     int        `json:"version"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

type TimelineEvent struct {
	ID         int64          `json:"id"`
	EventType  string         `json:"eventType"`
	FromStatus *Status        `json:"fromStatus,omitempty"`
	ToStatus   *Status        `json:"toStatus,omitempty"`
	Message    string         `json:"message"`
	Metadata   map[string]any `json:"metadata"`
	CreatedAt  time.Time      `json:"createdAt"`
}

type ListResult struct {
	Items []Order
	Total int64
}

func NewStore(db *pgxpool.Pool, leaseDuration time.Duration, maxRetries int, queueSLA time.Duration) *Store {
	return &Store{db: db, leaseDuration: leaseDuration, maxRetries: maxRetries, queueSLA: queueSLA}
}

const orderColumns = `order_no,customer_ref,product_code,player_uid,amount::text,status,retry_count,next_retry_at,expires_at,completed_at,version,created_at,updated_at`

type scanner interface{ Scan(...any) error }

func scanOrder(row scanner) (Order, error) {
	var o Order
	err := row.Scan(&o.OrderNo, &o.CustomerRef, &o.ProductCode, &o.PlayerUID, &o.Amount, &o.Status, &o.RetryCount, &o.NextRetryAt, &o.ExpiresAt, &o.CompletedAt, &o.Version, &o.CreatedAt, &o.UpdatedAt)
	return o, err
}

func (s *Store) Create(ctx context.Context, key, requestHash string, input CreateInput, expiresAt time.Time) (Order, bool, error) {
	for attempt := 0; attempt < 5; attempt++ {
		tx, err := s.db.Begin(ctx)
		if err != nil {
			return Order{}, false, err
		}
		orderNo := newOrderNo(time.Now())
		row := tx.QueryRow(ctx, `INSERT INTO orders(order_no,idempotency_key,request_hash,customer_ref,product_code,player_uid,amount,status,expires_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,'PENDING_PAYMENT',$8)
			ON CONFLICT(idempotency_key) DO NOTHING RETURNING `+orderColumns,
			orderNo, key, requestHash, input.CustomerRef, input.ProductCode, input.PlayerUID, input.Amount, expiresAt)
		o, scanErr := scanOrder(row)
		if scanErr == nil {
			_, err = tx.Exec(ctx, `INSERT INTO order_timeline(order_id,event_type,to_status,message,metadata)
				SELECT id,'ORDER_CREATED','PENDING_PAYMENT','Order created; waiting for payment',jsonb_build_object('idempotencyKey',$2::text) FROM orders WHERE order_no=$1`, orderNo, key)
			if err == nil {
				err = tx.Commit(ctx)
			}
			if err != nil {
				_ = tx.Rollback(ctx)
				return Order{}, false, err
			}
			return o, false, nil
		}
		_ = tx.Rollback(ctx)
		if !errors.Is(scanErr, pgx.ErrNoRows) {
			// A rare order number collision is safe to retry.
			if isUniqueViolation(scanErr, "orders_order_no_key") {
				continue
			}
			return Order{}, false, scanErr
		}

		var storedHash string
		o, err = scanOrder(s.db.QueryRow(ctx, `SELECT `+orderColumns+` FROM orders WHERE idempotency_key=$1`, key))
		if err != nil {
			return Order{}, false, err
		}
		if err = s.db.QueryRow(ctx, `SELECT request_hash FROM orders WHERE idempotency_key=$1`, key).Scan(&storedHash); err != nil {
			return Order{}, false, err
		}
		if storedHash != requestHash {
			return Order{}, false, ErrConflict
		}
		return o, true, nil
	}
	return Order{}, false, fmt.Errorf("could not generate a unique order number")
}

func newOrderNo(now time.Time) string {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("ORD-%013d-%03d", now.UnixMilli(), binary.BigEndian.Uint16(b[:])%1000)
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505" && constraint != ""
}

func (s *Store) Get(ctx context.Context, orderNo string) (Order, error) {
	o, err := scanOrder(s.db.QueryRow(ctx, `SELECT `+orderColumns+` FROM orders WHERE order_no=$1`, orderNo))
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrNotFound
	}
	return o, err
}

func (s *Store) List(ctx context.Context, tab string, page, pageSize int) (ListResult, error) {
	history := tab == "history"
	statuses := []string{string(PendingPayment), string(Queued), string(InProgress), string(Delayed)}
	if history {
		statuses = []string{string(Completed), string(Failed), string(Expired), string(Refunded)}
	}
	var total int64
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM orders WHERE status=ANY($1)`, statuses).Scan(&total); err != nil {
		return ListResult{}, err
	}
	rows, err := s.db.Query(ctx, `SELECT `+orderColumns+` FROM orders WHERE status=ANY($1) ORDER BY created_at DESC,order_no DESC LIMIT $2 OFFSET $3`, statuses, pageSize, (page-1)*pageSize)
	if err != nil {
		return ListResult{}, err
	}
	defer rows.Close()
	items := make([]Order, 0)
	for rows.Next() {
		o, scanErr := scanOrder(rows)
		if scanErr != nil {
			return ListResult{}, scanErr
		}
		items = append(items, o)
	}
	return ListResult{Items: items, Total: total}, rows.Err()
}

func (s *Store) Timeline(ctx context.Context, orderNo string) ([]TimelineEvent, error) {
	rows, err := s.db.Query(ctx, `SELECT t.id,t.event_type,t.from_status,t.to_status,t.message,t.metadata,t.created_at
		FROM order_timeline t JOIN orders o ON o.id=t.order_id WHERE o.order_no=$1 ORDER BY t.id`, orderNo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]TimelineEvent, 0)
	for rows.Next() {
		var e TimelineEvent
		var raw []byte
		if err = rows.Scan(&e.ID, &e.EventType, &e.FromStatus, &e.ToStatus, &e.Message, &raw, &e.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(raw, &e.Metadata)
		events = append(events, e)
	}
	if len(events) == 0 {
		if _, getErr := s.Get(ctx, orderNo); getErr != nil {
			return nil, getErr
		}
	}
	return events, rows.Err()
}

func (s *Store) ReportIssue(ctx context.Context, orderNo, issueType, description string) (string, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var id, orderID string
	var status Status
	if err = tx.QueryRow(ctx, `SELECT id::text,status FROM orders WHERE order_no=$1 FOR UPDATE`, orderNo).Scan(&orderID, &status); errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	} else if err != nil {
		return "", err
	}
	if err = tx.QueryRow(ctx, `INSERT INTO issue_reports(order_id,issue_type,description) VALUES($1,$2,$3) RETURNING id::text`, orderID, issueType, description).Scan(&id); err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `INSERT INTO order_timeline(order_id,event_type,from_status,to_status,message,metadata) VALUES($1,'ISSUE_REPORTED',$2,$2,'Customer reported an issue',jsonb_build_object('issueId',$3::text,'issueType',$4::text))`, orderID, status, id, issueType)
	if err != nil {
		return "", err
	}
	return id, tx.Commit(ctx)
}

func (s *Store) Claim(ctx context.Context, workerID string) (*Order, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var id string
	var from Status
	err = tx.QueryRow(ctx, `SELECT id::text,status FROM orders WHERE status='QUEUED' AND (next_retry_at IS NULL OR next_retry_at<=now()) ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&id, &from)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	o, err := scanOrder(tx.QueryRow(ctx, `UPDATE orders SET status='IN_PROGRESS',lease_owner=$2,lease_expires_at=now()+($3 * interval '1 second'),last_heartbeat_at=now(),updated_at=now(),version=version+1 WHERE id=$1 RETURNING `+orderColumns, id, workerID, s.leaseDuration.Seconds()))
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO payment_attempts(order_id,attempt_no,status) SELECT id,retry_count+1,'STARTED' FROM orders WHERE id=$1 ON CONFLICT(order_id,attempt_no) DO NOTHING`, id)
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO order_timeline(order_id,event_type,from_status,to_status,message,metadata) VALUES($1,'BOT_CLAIMED',$2,'IN_PROGRESS','Bot claimed order',jsonb_build_object('workerId',$3::text))`, id, from, workerID)
	}
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &o, nil
}

func (s *Store) Heartbeat(ctx context.Context, orderNo, workerID string) error {
	command, err := s.db.Exec(ctx, `UPDATE orders SET last_heartbeat_at=now(),lease_expires_at=now()+($3 * interval '1 second'),updated_at=now() WHERE order_no=$1 AND status='IN_PROGRESS' AND lease_owner=$2 AND lease_expires_at>now()`, orderNo, workerID, s.leaseDuration.Seconds())
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

func (s *Store) Result(ctx context.Context, orderNo, workerID, outcome, errorCode, message string) (Order, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Order{}, err
	}
	defer tx.Rollback(ctx)
	var id string
	var from Status
	var retry int
	var leaseOwner *string
	var leaseExpiry *time.Time
	err = tx.QueryRow(ctx, `SELECT id::text,status,retry_count,lease_owner,lease_expires_at FROM orders WHERE order_no=$1 FOR UPDATE`, orderNo).Scan(&id, &from, &retry, &leaseOwner, &leaseExpiry)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrNotFound
	}
	if err != nil {
		return Order{}, err
	}
	if from != InProgress || leaseOwner == nil || *leaseOwner != workerID || leaseExpiry == nil || leaseExpiry.Before(time.Now()) {
		return Order{}, ErrLeaseLost
	}

	attemptNo := retry + 1
	to, eventType := Completed, "BOT_COMPLETED"
	nextRetry := any(nil)
	switch outcome {
	case "success":
	case "permanent_failure":
		to, eventType = Failed, "PERMANENT_FAILURE"
	case "transient_failure":
		retry++
		if retry >= s.maxRetries {
			to, eventType = Delayed, "RETRY_EXHAUSTED"
		} else {
			to, eventType = Queued, "RETRY_SCHEDULED"
			nextRetry = time.Now().Add(RetryDelay(retry))
		}
	default:
		return Order{}, fmt.Errorf("unknown outcome")
	}
	if !CanTransition(from, to) {
		return Order{}, ErrInvalidTransition
	}
	completedAt := any(nil)
	if to == Completed {
		completedAt = time.Now()
	}
	o, err := scanOrder(tx.QueryRow(ctx, `UPDATE orders SET status=$2,retry_count=$3,next_retry_at=$4,lease_owner=NULL,lease_expires_at=NULL,last_heartbeat_at=NULL,completed_at=$5,updated_at=now(),version=version+1 WHERE id=$1 RETURNING `+orderColumns, id, to, retry, nextRetry, completedAt))
	if err != nil {
		return Order{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE payment_attempts SET status=CASE WHEN $2='success' THEN 'SUCCEEDED' ELSE 'FAILED' END WHERE order_id=$1 AND attempt_no=$3`, id, outcome, attemptNo)
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO order_timeline(order_id,event_type,from_status,to_status,message,metadata) VALUES($1,$2,$3,$4,$5,jsonb_build_object('workerId',$6::text,'errorCode',$7::text))`, id, eventType, from, to, fallback(message, string(to)), workerID, errorCode)
	}
	if err != nil {
		return Order{}, err
	}
	return o, tx.Commit(ctx)
}

func (s *Store) BankWebhook(ctx context.Context, eventID, orderNo, paymentStatus string, payload []byte) (string, bool, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback(ctx)
	var existing string
	err = tx.QueryRow(ctx, `SELECT result FROM webhook_events WHERE source='bank' AND event_id=$1`, eventID).Scan(&existing)
	if err == nil {
		return existing, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, err
	}
	var id string
	var from Status
	if err = tx.QueryRow(ctx, `SELECT id::text,status FROM orders WHERE order_no=$1 FOR UPDATE`, orderNo).Scan(&id, &from); errors.Is(err, pgx.ErrNoRows) {
		return "", false, ErrNotFound
	} else if err != nil {
		return "", false, err
	}
	result := "ignored_out_of_order"
	var to *Status
	if paymentStatus == "paid" && from == PendingPayment {
		value := Queued
		to, result = &value, "queued"
	} else if paymentStatus == "paid" && from == Expired {
		value := Refunded
		to, result = &value, "late_payment_refunded"
	} else if paymentStatus == "refunded" && (from == Completed || from == Failed || from == Delayed) {
		value := Refunded
		to, result = &value, "refunded"
	}
	if to != nil {
		_, err = tx.Exec(ctx, `UPDATE orders SET status=$2,updated_at=now(),version=version+1 WHERE id=$1`, id, *to)
		if err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO order_timeline(order_id,event_type,from_status,to_status,message,metadata) VALUES($1,'BANK_WEBHOOK',$2,$3,$4,jsonb_build_object('eventId',$5::text))`, id, from, *to, result, eventID)
		}
	}
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO webhook_events(source,event_id,order_no,payload,result) VALUES('bank',$1,$2,$3,$4)`, eventID, orderNo, payload, result)
	}
	if err != nil {
		return "", false, err
	}
	return result, false, tx.Commit(ctx)
}

func (s *Store) CorrectUIDAndRetry(ctx context.Context, orderNo, uid string) (Order, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Order{}, err
	}
	defer tx.Rollback(ctx)
	var id string
	var from Status
	if err = tx.QueryRow(ctx, `SELECT id::text,status FROM orders WHERE order_no=$1 FOR UPDATE`, orderNo).Scan(&id, &from); errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrNotFound
	} else if err != nil {
		return Order{}, err
	}
	if from != Failed && from != Delayed {
		return Order{}, ErrInvalidTransition
	}
	o, err := scanOrder(tx.QueryRow(ctx, `UPDATE orders SET player_uid=$2,status='QUEUED',retry_count=0,next_retry_at=now(),updated_at=now(),version=version+1 WHERE id=$1 RETURNING `+orderColumns, id, uid))
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO order_timeline(order_id,event_type,from_status,to_status,message) VALUES($1,'UID_CORRECTED',$2,'QUEUED','Player UID corrected; order requeued')`, id, from)
	}
	if err != nil {
		return Order{}, err
	}
	return o, tx.Commit(ctx)
}

type SweepStats struct {
	Expired int64 `json:"expired"`
	Retried int64 `json:"retried"`
	Delayed int64 `json:"delayed"`
	Alerts  int64 `json:"alerts"`
}

func (s *Store) Sweep(ctx context.Context) (SweepStats, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return SweepStats{}, err
	}
	defer tx.Rollback(ctx)
	stats := SweepStats{}
	rows, err := tx.Query(ctx, `UPDATE orders SET status='EXPIRED',updated_at=now(),version=version+1 WHERE status='PENDING_PAYMENT' AND expires_at<=now() RETURNING id`)
	if err != nil {
		return stats, err
	}
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		_, _ = tx.Exec(ctx, `INSERT INTO order_timeline(order_id,event_type,from_status,to_status,message) VALUES($1,'PAYMENT_EXPIRED','PENDING_PAYMENT','EXPIRED','Payment window expired')`, id)
		stats.Expired++
	}
	rows.Close()

	rows, err = tx.Query(ctx, `SELECT id::text,retry_count FROM orders WHERE status='IN_PROGRESS' AND lease_expires_at<=now() FOR UPDATE SKIP LOCKED`)
	if err != nil {
		return stats, err
	}
	type stale struct {
		id    string
		retry int
	}
	staleOrders := []stale{}
	for rows.Next() {
		var item stale
		_ = rows.Scan(&item.id, &item.retry)
		staleOrders = append(staleOrders, item)
	}
	rows.Close()
	for _, item := range staleOrders {
		item.retry++
		to, event := Queued, "LEASE_EXPIRED_RETRY"
		next := any(time.Now().Add(RetryDelay(item.retry)))
		if item.retry >= s.maxRetries {
			to, event, next = Delayed, "LEASE_RETRY_EXHAUSTED", nil
			stats.Delayed++
		} else {
			stats.Retried++
		}
		_, err = tx.Exec(ctx, `UPDATE orders SET status=$2,retry_count=$3,next_retry_at=$4,lease_owner=NULL,lease_expires_at=NULL,last_heartbeat_at=NULL,updated_at=now(),version=version+1 WHERE id=$1`, item.id, to, item.retry, next)
		if err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO order_timeline(order_id,event_type,from_status,to_status,message,metadata) VALUES($1,$2,'IN_PROGRESS',$3,'Bot lease expired',jsonb_build_object('retryCount',$4::int))`, item.id, event, to, item.retry)
		}
		if err != nil {
			return stats, err
		}
	}
	command, err := tx.Exec(ctx, `INSERT INTO order_alerts(order_id,alert_type,message)
		SELECT id,'QUEUE_SLA','Order has waited in queue beyond SLA' FROM orders o
		WHERE status='QUEUED' AND created_at<now()-($1 * interval '1 second')
		AND NOT EXISTS(SELECT 1 FROM order_alerts a WHERE a.order_id=o.id AND a.alert_type='QUEUE_SLA' AND a.resolved_at IS NULL)
		ON CONFLICT DO NOTHING`, s.queueSLA.Seconds())
	if err != nil {
		return stats, err
	}
	stats.Alerts = command.RowsAffected()
	return stats, tx.Commit(ctx)
}

func fallback(value, other string) string {
	if value != "" {
		return value
	}
	return other
}
