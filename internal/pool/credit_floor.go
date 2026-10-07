package pool

import (
	"math"
	"strings"
	"time"
)

type modelPrice struct {
	paid  bool
	until time.Time
}

func (accountPool *Pool) SetCreditFloor(floor float64) {
	if floor < 0 || math.IsNaN(floor) || math.IsInf(floor, 0) {
		floor = 0
	}
	accountPool.mu.Lock()
	defer accountPool.mu.Unlock()
	accountPool.creditFloor = floor
	accountPool.notifyChange()
}

func (accountPool *Pool) CreditFloor() float64 {
	accountPool.mu.RLock()
	defer accountPool.mu.RUnlock()
	return accountPool.creditFloor
}

func (accountPool *Pool) SetModelRates(route string, rates map[string]float64, validUntil time.Time) {
	accountPool.mu.Lock()
	defer accountPool.mu.Unlock()
	if accountPool.modelRates == nil {
		accountPool.modelRates = make(map[string]modelPrice)
	}
	now := time.Now()
	for key, price := range accountPool.modelRates {
		if now.After(price.until) || strings.HasPrefix(key, route+"\x00") {
			delete(accountPool.modelRates, key)
		}
	}
	for model, rate := range rates {
		if model == "" || rate < 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
			continue
		}
		if len(accountPool.modelRates) >= 2048 {
			break
		}
		accountPool.modelRates[route+"\x00"+model] = modelPrice{paid: rate > 0, until: validUntil}
	}
}

func (accountPool *Pool) NoteModelCharge(uid, route, model string, credits float64) {
	if model == "" || credits < 0 || math.IsNaN(credits) || math.IsInf(credits, 0) {
		return
	}
	accountPool.mu.Lock()
	defer accountPool.mu.Unlock()
	account, exists := accountPool.byUID[uid]
	if !exists {
		return
	}
	if account.modelCharges == nil {
		account.modelCharges = make(map[string]modelPrice)
	}
	now := time.Now()
	for key, price := range account.modelCharges {
		if now.After(price.until) {
			delete(account.modelCharges, key)
		}
	}
	key := route + "\x00" + model
	if _, exists := account.modelCharges[key]; !exists && len(account.modelCharges) >= 256 {
		return
	}
	account.modelCharges[key] = modelPrice{paid: credits > 0, until: now.Add(10 * time.Minute)}
}

func (accountPool *Pool) floorBlocked(account *entry, route, model string, now time.Time) bool {
	if accountPool.creditFloor <= 0 || model == "" || !account.creditsKnown || float64(account.credits)-account.creditFrac >= accountPool.creditFloor {
		return false
	}
	key := route + "\x00" + model
	if price, exists := account.modelCharges[key]; exists && now.Before(price.until) {
		return price.paid
	}
	price, exists := accountPool.modelRates[key]
	return exists && now.Before(price.until) && price.paid
}
