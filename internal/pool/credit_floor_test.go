package pool

import (
	"math"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

func TestCreditFloorSelection(test *testing.T) {
	for _, scenario := range []struct {
		name                             string
		floor, balance, rate             float64
		knownBalance, knownRate, blocked bool
	}{
		{"disabled", 0, 5, 1, true, true, false},
		{"paid", 10, 5, 1, true, true, true},
		{"fraction", 10, 9.75, 1, true, true, true},
		{"threshold", 10, 10, 1, true, true, false},
		{"free", 10, 5, 0, true, true, false},
		{"unknown_balance", 10, 0, 1, false, true, false},
		{"unknown_rate", 10, 5, 0, true, false, false},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			accountPool := New("")
			accountPool.Add(&auth.Auth{UID: "account"})
			accountPool.SetCreditFloor(scenario.floor)
			if scenario.knownBalance {
				accountPool.SetCredits("account", int64(math.Ceil(scenario.balance)))
				accountPool.DeductCredits("account", math.Ceil(scenario.balance)-scenario.balance)
			}
			if scenario.knownRate {
				accountPool.SetModelRates("cn", map[string]float64{"model": scenario.rate}, time.Now().Add(time.Hour))
			}
			if blocked := accountPool.PickExcludingForRoute(nil, "model", "cn") == nil; blocked != scenario.blocked {
				test.Fatalf("normal blocked=%v", blocked)
			}
			if blocked := accountPool.PickByUIDForModel("account", "model", "cn") == nil; blocked != scenario.blocked {
				test.Fatalf("sticky blocked=%v", blocked)
			}
			accountPool.Cooldown("account", CoolSoft, time.Hour, "test")
			if blocked := accountPool.PickExcludingForRoute(nil, "model", "cn") == nil; blocked != scenario.blocked {
				test.Fatalf("fallback blocked=%v", blocked)
			}
		})
	}
}

func TestCreditFloorEvidenceAndRoutes(test *testing.T) {
	accountPool := New("")
	accountPool.Add(&auth.Auth{UID: "first"})
	accountPool.Add(&auth.Auth{UID: "second"})
	accountPool.SetCredits("first", 5)
	accountPool.SetCredits("second", 5)
	accountPool.SetCreditFloor(10)
	accountPool.SetModelRates("workbuddy", map[string]float64{"paid": 2}, time.Now().Add(time.Hour))
	if accountPool.PickByUIDForModel("first", "paid", "codebuddy") == nil {
		test.Fatal("price leaked across routes")
	}
	accountPool.NoteModelCharge("first", "workbuddy", "paid", 0)
	if accountPool.PickByUIDForModel("first", "paid", "workbuddy") == nil {
		test.Fatal("observed free rate not respected")
	}
	if accountPool.PickByUIDForModel("second", "paid", "workbuddy") != nil {
		test.Fatal("observed price leaked across accounts")
	}
	accountPool.mu.Lock()
	accountPool.byUID["first"].modelCharges["workbuddy\x00paid"] = modelPrice{until: time.Now().Add(-time.Hour)}
	accountPool.mu.Unlock()
	if accountPool.PickByUIDForModel("first", "paid", "workbuddy") != nil {
		test.Fatal("expired evidence overrides catalog")
	}
	accountPool.SetModelRates("workbuddy", map[string]float64{"paid": 2}, time.Now().Add(-time.Hour))
	if accountPool.PickByUIDForModel("first", "paid", "workbuddy") == nil {
		test.Fatal("expired catalog blocks unknown price")
	}
	accountPool.NoteModelCharge("first", "workbuddy", "paid", 1)
	if accountPool.PickByUIDForModel("first", "paid", "workbuddy") != nil {
		test.Fatal("observed paid rate ignored")
	}
	accountPool.SetCreditFloor(-1)
	if accountPool.CreditFloor() != 0 {
		test.Fatal("negative floor not disabled")
	}
}
