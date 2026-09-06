package ratelimit

import (
	"testing"
	"time"
)

// A peek must spend nothing and refill nothing.
func TestExhaustedIsAPeek(t *testing.T) {
	l := New(Limit{Rate: 60, Window: time.Minute, Burst: 2}, 0)
	if ex, _ := l.Exhausted("c"); ex {
		t.Fatal("an unseen key cannot be exhausted")
	}
	l.Allow("c")
	l.Allow("c") // burst spent
	ex, retry := l.Exhausted("c")
	if !ex {
		t.Fatal("after spending the burst the key must read as exhausted")
	}
	if retry <= 0 || retry > time.Second+time.Millisecond {
		t.Errorf("retry after = %v, want about one token's refill", retry)
	}
	// Asking did not change the answer.
	if ex2, _ := l.Exhausted("c"); !ex2 {
		t.Error("a peek must not refill the bucket")
	}
	if ok, _ := l.Allow("c"); ok {
		t.Error("a peek must not hand a token back")
	}
}
