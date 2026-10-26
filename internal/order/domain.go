package order

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"time"
)

type Status string

const (
	PendingPayment Status = "PENDING_PAYMENT"
	Queued         Status = "QUEUED"
	InProgress     Status = "IN_PROGRESS"
	Completed      Status = "COMPLETED"
	Delayed        Status = "DELAYED"
	Failed         Status = "FAILED"
	Expired        Status = "EXPIRED"
	Refunded       Status = "REFUNDED"
)

var orderNoPattern = regexp.MustCompile(`^ORD-[0-9]{13}-[0-9]{3}$`)

var transitions = map[Status]map[Status]bool{
	PendingPayment: {Queued: true, Expired: true},
	Queued:         {InProgress: true, Delayed: true, Failed: true},
	InProgress:     {Completed: true, Queued: true, Delayed: true, Failed: true},
	Delayed:        {Queued: true, Failed: true, Refunded: true},
	Failed:         {Queued: true, Refunded: true},
	Expired:        {Refunded: true},
	Completed:      {Refunded: true},
	Refunded:       {},
}

func ValidOrderNo(value string) bool { return orderNoPattern.MatchString(value) }

func CanTransition(from, to Status) bool { return transitions[from][to] }

func IsHistory(status Status) bool {
	return status == Completed || status == Failed || status == Expired || status == Refunded
}

func RetryDelay(retry int) time.Duration {
	if retry < 1 {
		retry = 1
	}
	if retry > 6 {
		retry = 6
	}
	return time.Duration(1<<(retry-1)) * 5 * time.Second
}

type CreateInput struct {
	CustomerRef string `json:"customerRef"`
	ProductCode string `json:"productCode"`
	PlayerUID   string `json:"playerUid"`
	Amount      string `json:"amount"`
}

func (i CreateInput) Validate() map[string]string {
	fields := map[string]string{}
	if len(i.CustomerRef) < 1 || len(i.CustomerRef) > 100 {
		fields["customerRef"] = "must contain 1-100 characters"
	}
	if len(i.ProductCode) < 1 || len(i.ProductCode) > 100 {
		fields["productCode"] = "must contain 1-100 characters"
	}
	if len(i.PlayerUID) < 1 || len(i.PlayerUID) > 100 {
		fields["playerUid"] = "must contain 1-100 characters"
	}
	if !regexp.MustCompile(`^[0-9]+(\.[0-9]{1,2})?$`).MatchString(i.Amount) || i.Amount == "0" || i.Amount == "0.00" || i.Amount == "0.0" {
		fields["amount"] = "must be a positive decimal with at most 2 decimal places"
	}
	return fields
}

func RequestHash(input CreateInput) string {
	b, _ := json.Marshal(input)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

var ErrNotFound = errors.New("order not found")
var ErrConflict = errors.New("conflict")
var ErrInvalidTransition = errors.New("invalid state transition")
var ErrLeaseLost = errors.New("worker does not own active lease")
