package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

// PackageAmounts uses decimal strings so neither aggregation nor JSON clients
// lose the fractional credits returned by billing's *Precise fields.
type PackageAmounts struct {
	Total     string `json:"total"`
	Used      string `json:"used"`
	Remaining string `json:"remaining"`
}

type PackageTotal struct {
	Unit string `json:"unit"`
	PackageAmounts
}

// AccountPackage is deliberately a whitelist: billing also returns account,
// order and binding metadata which the console does not need.
type AccountPackage struct {
	Name    string `json:"name"`
	Product string `json:"product"`
	Unit    string `json:"unit"`
	Status  int    `json:"status"`
	Basis   string `json:"basis"` // cycle or package
	Precise bool   `json:"precise"`
	PackageAmounts
	CycleStart  string `json:"cycle_start,omitempty"` // upstream wall time; zone unspecified
	CycleEnd    string `json:"cycle_end,omitempty"`
	UsableFrom  string `json:"usable_from,omitempty"` // RFC3339, converted from epoch milliseconds
	UsableUntil string `json:"usable_until,omitempty"`
	ExpiredTime string `json:"expired_time,omitempty"` // validated upstream wall time
}

type AccountPackages struct {
	CheckedAt time.Time        `json:"checked_at"`
	Count     int              `json:"count"`
	Totals    []PackageTotal   `json:"totals"`
	Packages  []AccountPackage `json:"packages"`
}

type resourcePackage struct {
	Name               string      `json:"PackageName"`
	Product            string      `json:"SubProductName"`
	Unit               string      `json:"CapacityUnit"`
	Status             int         `json:"Status"`
	Size               json.Number `json:"CapacitySize"`
	Remain             json.Number `json:"CapacityRemain"`
	Used               json.Number `json:"CapacityUsed"`
	SizePrecise        string      `json:"CapacitySizePrecise"`
	RemainPrecise      string      `json:"CapacityRemainPrecise"`
	UsedPrecise        string      `json:"CapacityUsedPrecise"`
	CycleSize          json.Number `json:"CycleCapacitySize"`
	CycleRemain        json.Number `json:"CycleCapacityRemain"`
	CycleUsed          json.Number `json:"CycleCapacityUsed"`
	CycleSizePrecise   string      `json:"CycleCapacitySizePrecise"`
	CycleRemainPrecise string      `json:"CycleCapacityRemainPrecise"`
	CycleUsedPrecise   string      `json:"CycleCapacityUsedPrecise"`
	CycleStart         string      `json:"CycleStartTime"`
	CycleEnd           string      `json:"CycleEndTime"`
	DeductionStart     int64       `json:"DeductionStartTime"`
	DeductionEnd       int64       `json:"DeductionEndTime"`
	ExpiredTime        string      `json:"ExpiredTime"`
}

// AccountPackagesContext reads every page of the currently nonexpired resource
// set, including exhausted packages. These totals are not lifetime spending.
// It is intentionally independent of UserResource and never updates pool state.
func (c *Client) AccountPackagesContext(ctx context.Context, a *auth.Auth) (*AccountPackages, error) {
	const pageSize, maxPackages = 100, 10000
	now := time.Now()
	a = a.Snapshot()
	out := &AccountPackages{Packages: []AccountPackage{}, Totals: []PackageTotal{}}
	total := -1
	sums := map[string][3]*big.Rat{}
	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := c.billingJSONContext(ctx, a, http.MethodPost, billingMeterPath, map[string]any{
			"PageNumber": page, "PageSize": pageSize, "ProductCode": "p_tcaca", "Status": []int{0, 3},
			"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
			"PackageEndTimeRangeEnd":   now.AddDate(101, 0, 0).Format("2006-01-02 15:04:05"),
		})
		if err != nil {
			return nil, err
		}
		var resp struct {
			Response struct {
				Data *struct {
					TotalCount *int              `json:"TotalCount"`
					Accounts   []resourcePackage `json:"Accounts"`
				} `json:"Data"`
			} `json:"Response"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			return nil, fmt.Errorf("packages: invalid response")
		}
		d := resp.Response.Data
		if d == nil || d.TotalCount == nil || *d.TotalCount < 0 || *d.TotalCount > maxPackages {
			return nil, fmt.Errorf("packages: missing or invalid total count")
		}
		if total < 0 {
			total = *d.TotalCount
		}
		if total != *d.TotalCount || len(d.Accounts) > pageSize || len(out.Packages)+len(d.Accounts) > total {
			return nil, fmt.Errorf("packages: resource set changed; retry query")
		}
		for _, raw := range d.Accounts {
			p, values, err := normalizePackage(raw)
			if err != nil {
				return nil, err
			}
			out.Packages = append(out.Packages, p)
			sum, ok := sums[p.Unit]
			if !ok {
				sum = [3]*big.Rat{new(big.Rat), new(big.Rat), new(big.Rat)}
			}
			for i := range sum {
				sum[i].Add(sum[i], values[i])
			}
			sums[p.Unit] = sum
		}
		if len(out.Packages) == total {
			break
		}
		if len(d.Accounts) != pageSize {
			return nil, fmt.Errorf("packages: incomplete page")
		}
	}
	units := make([]string, 0, len(sums))
	for unit := range sums {
		units = append(units, unit)
	}
	sort.Strings(units)
	for _, unit := range units {
		out.Totals = append(out.Totals, PackageTotal{Unit: unit, PackageAmounts: packageAmounts(sums[unit])})
	}
	out.Count, out.CheckedAt = len(out.Packages), time.Now()
	return out, nil
}

// Bounded plain decimals reject NaN, exponents and malformed precise values
// rather than silently falling back to a rounded balance. Eighteen fractional
// places are retained exactly throughout addition and subtraction.
var packageDecimal = regexp.MustCompile(`^-?[0-9]{1,40}(\.[0-9]{1,18})?$`)

func resourceDecimal(precise string, fallback json.Number) (*big.Rat, error) {
	value := strings.TrimSpace(precise)
	if value == "" {
		value = string(fallback)
	}
	if value == "" {
		return nil, nil
	}
	if !packageDecimal.MatchString(value) {
		return nil, fmt.Errorf("packages: invalid credit amount")
	}
	n, ok := new(big.Rat).SetString(value)
	if !ok {
		return nil, fmt.Errorf("packages: invalid credit amount")
	}
	return n, nil
}

func normalizePackage(r resourcePackage) (AccountPackage, [3]*big.Rat, error) {
	p := AccountPackage{Name: r.Name, Product: r.Product, Unit: strings.TrimSpace(r.Unit), Status: r.Status, Basis: "package"}
	// Bonus packs use singular "credit", subscriptions use "credits" for
	// the same currency. Other resource units must remain separate.
	if strings.EqualFold(p.Unit, "credit") || strings.EqualFold(p.Unit, "credits") {
		p.Unit = "credits"
	}
	if p.Unit == "" {
		p.Unit = "unknown"
	}
	var values [3]*big.Rat
	precise := [3]string{r.CycleSizePrecise, r.CycleUsedPrecise, r.CycleRemainPrecise}
	fallback := [3]json.Number{r.CycleSize, r.CycleUsed, r.CycleRemain}
	hasCycle := false
	for i := range values {
		v, err := resourceDecimal(precise[i], fallback[i])
		if err != nil {
			return p, values, err
		}
		values[i] = v
		if v != nil && v.Sign() != 0 {
			hasCycle = true
		}
	}
	if hasCycle {
		p.Basis = "cycle"
	} else {
		precise = [3]string{r.SizePrecise, r.UsedPrecise, r.RemainPrecise}
		fallback = [3]json.Number{r.Size, r.Used, r.Remain}
		for i := range values {
			v, err := resourceDecimal(precise[i], fallback[i])
			if err != nil {
				return p, values, err
			}
			values[i] = v
		}
		// An explicitly all-zero cycle with no package amounts is valid too.
		if values[0] == nil && values[1] == nil && values[2] == nil && (r.CycleSizePrecise != "" || r.CycleSize != "") {
			p.Basis = "cycle"
			precise = [3]string{r.CycleSizePrecise, r.CycleUsedPrecise, r.CycleRemainPrecise}
			fallback = [3]json.Number{r.CycleSize, r.CycleUsed, r.CycleRemain}
			for i := range values {
				values[i], _ = resourceDecimal(precise[i], fallback[i])
			}
		}
	}
	if values[0] == nil || values[2] == nil {
		return p, values, fmt.Errorf("packages: missing capacity or remaining amount")
	}
	p.Precise = precise[0] != "" && precise[2] != ""
	// Some responses omit used or supply only its truncated integer, while
	// providing precise total and remaining. Derive used from those exact values.
	if values[1] == nil || (p.Precise && precise[1] == "") {
		values[1] = new(big.Rat).Sub(values[0], values[2])
	}
	if values[0].Sign() < 0 || values[1].Sign() < 0 {
		return p, values, fmt.Errorf("packages: negative capacity or usage")
	}
	// Billing can briefly overdraw a package; remaining spendable credits floor at zero.
	if values[2].Sign() < 0 {
		values[2] = new(big.Rat)
	}
	p.PackageAmounts = packageAmounts(values)
	p.CycleStart, p.CycleEnd = packageWallTime(r.CycleStart), packageWallTime(r.CycleEnd)
	p.UsableFrom, p.UsableUntil = packageEpoch(r.DeductionStart), packageEpoch(r.DeductionEnd)
	p.ExpiredTime = packageWallTime(r.ExpiredTime)
	return p, values, nil
}

func packageDecimalString(n *big.Rat) string {
	s := strings.TrimRight(strings.TrimRight(n.FloatString(18), "0"), ".")
	if s == "-0" {
		return "0"
	}
	return s
}

func packageAmounts(v [3]*big.Rat) PackageAmounts {
	return PackageAmounts{Total: packageDecimalString(v[0]), Used: packageDecimalString(v[1]), Remaining: packageDecimalString(v[2])}
}

func packageWallTime(s string) string {
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil && t.Year() < 9999 {
			return s
		}
	}
	return ""
}

func packageEpoch(ms int64) string {
	if ms <= 0 {
		return ""
	}
	t := time.UnixMilli(ms).UTC()
	if t.Year() >= 9999 {
		return ""
	}
	return t.Format(time.RFC3339)
}
