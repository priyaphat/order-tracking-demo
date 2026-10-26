package order

import (
	"testing"
	"time"
)

func TestOrderNumberValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		value string
		valid bool
	}{{"ORD-2587965432159-458", true}, {"ord-2587965432159-458", false}, {"ORD-123-458", false}, {"ORD-2587965432159-45A", false}} {
		if got := ValidOrderNo(tc.value); got != tc.valid {
			t.Fatalf("ValidOrderNo(%q) = %v; want %v", tc.value, got, tc.valid)
		}
	}
}

func TestStateMachine(t *testing.T) {
	t.Parallel()
	if !CanTransition(PendingPayment, Queued) || !CanTransition(InProgress, Completed) {
		t.Fatal("normal transitions must be allowed")
	}
	if CanTransition(Completed, InProgress) || CanTransition(PendingPayment, Completed) {
		t.Fatal("invalid transitions must be rejected")
	}
}

func TestRetryDelayIsExponentialAndCapped(t *testing.T) {
	t.Parallel()
	if RetryDelay(1) != 5*time.Second || RetryDelay(2) != 10*time.Second || RetryDelay(3) != 20*time.Second {
		t.Fatal("unexpected exponential backoff")
	}
	if RetryDelay(20) != 160*time.Second {
		t.Fatal("backoff must be capped")
	}
}

func TestCreateValidation(t *testing.T) {
	t.Parallel()
	valid := CreateInput{CustomerRef: "C-1", ProductCode: "GAME-100", PlayerUID: "9988", Amount: "100.50"}
	if fields := valid.Validate(); len(fields) != 0 {
		t.Fatalf("valid input rejected: %v", fields)
	}
	valid.Amount = "10.999"
	if valid.Validate()["amount"] == "" {
		t.Fatal("amount scale must be validated")
	}
}
