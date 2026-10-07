package pool

import (
	"testing"
	"time"
	"workbuddy2api/internal/auth"
)

func TestRuntimeKeepsAccountAndInFlightState(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a"})
	p.SetCredits("a", 100)
	if !p.Acquire("a") {
		t.Fatal("acquire")
	}
	p.ApplyRuntime(RuntimeOptions{SelectionMode: SelectionHighestCredits, MaxInFlight: 1, CreditFloor: 5, IdleWeightPerHour: 1, IdleWeightMax: 10, BreakerThreshold: 3, BreakerCooldown: time.Minute, BreakerCooldownMax: time.Hour, SoftRateMax: time.Hour, FreezeMax: time.Hour})
	if p.Acquire("a") {
		t.Fatal("reload discarded active lease")
	}
	if v, _ := p.CreditsOf("a"); v != 100 {
		t.Fatal("reload discarded credits")
	}
	if p.SelectionMode() != SelectionHighestCredits {
		t.Fatal("policy not applied")
	}
	p.Release("a")
	if !p.Acquire("a") {
		t.Fatal("lease not released")
	}
	p.Release("a")
}
