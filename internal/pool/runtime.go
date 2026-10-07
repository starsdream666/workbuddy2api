package pool

import "time"

type RuntimeOptions struct {
	SelectionMode                                               string
	MaxInFlight, BreakerThreshold                               int
	CreditFloor, IdleWeightPerHour, IdleWeightMax               float64
	BreakerCooldown, BreakerCooldownMax, SoftRateMax, FreezeMax time.Duration
}

// ApplyRuntime installs one validated policy without replacing any account state.
func (p *Pool) ApplyRuntime(o RuntimeOptions) {
	p.mu.Lock()
	p.selectionMode = o.SelectionMode
	p.maxInFlight = o.MaxInFlight
	p.creditFloor = o.CreditFloor
	p.idleWeightPerHour, p.idleWeightMax = o.IdleWeightPerHour, o.IdleWeightMax
	p.breakerThreshold = o.BreakerThreshold
	p.breakerCooldown, p.breakerCooldownMax = o.BreakerCooldown, o.BreakerCooldownMax
	p.softRateMax, p.freezeMax = o.SoftRateMax, o.FreezeMax
	p.mu.Unlock()
	p.notifyChange()
}
