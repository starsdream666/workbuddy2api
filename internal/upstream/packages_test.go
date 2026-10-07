package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/realm"
)

type packageTransport func(*http.Request) (*http.Response, error)

func (f packageTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func packageClient(fn packageTransport) *Client {
	return &Client{HTTP: &http.Client{Transport: fn}, RealmDefault: realm.WB}
}

func packageResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestAccountPackagesPaginationAndPrecision(t *testing.T) {
	calls := 0
	client := packageClient(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "POST" || r.URL.Path != billingMeterPath || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatal("unexpected billing request")
		}
		var req struct {
			PageNumber, PageSize     int
			Status                   []int
			PackageEndTimeRangeBegin string
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.PageNumber != calls || req.PageSize != 100 || len(req.Status) != 2 || req.PackageEndTimeRangeBegin == "" {
			t.Fatalf("bad page request: %+v", req)
		}
		count := 100
		if calls == 2 {
			count = 1
		}
		rows := make([]string, count)
		for i := range rows {
			rows[i] = `{"PackageName":"Bonus Pack","CapacityUnit":"credits","CycleCapacitySize":30,"CycleCapacityRemain":16,"CycleCapacityUsed":13,"CycleCapacitySizePrecise":"30","CycleCapacityRemainPrecise":"16.49","CycleCapacityUsedPrecise":"13.51","CapacitySize":999,"CapacityRemain":999,"CycleEndTime":"2026-10-31 23:59:59","DeductionEndTime":2049587698000,"ExpiredTime":"9999-99-99 99:99:99","AccountId":"private-id","AccessToken":"private-token","BindRecords":[{"EndTime":"9999-99-99 99:99:99"}]}`
		}
		return packageResponse(fmt.Sprintf(`{"code":0,"data":{"Response":{"Data":{"TotalCount":101,"TotalDosage":116,"Accounts":[%s]}}}}`, strings.Join(rows, ","))), nil
	})
	result, err := client.AccountPackagesContext(context.Background(), &auth.Auth{AccessToken: "test-token", Realm: realm.WB})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || result.Count != 101 || len(result.Totals) != 1 {
		t.Fatalf("calls=%d result=%+v", calls, result)
	}
	want := PackageAmounts{Total: "3030", Used: "1364.51", Remaining: "1665.49"}
	if result.Totals[0].PackageAmounts != want {
		t.Fatalf("totals=%+v want=%+v", result.Totals[0], want)
	}
	p := result.Packages[0]
	if !p.Precise || p.Basis != "cycle" || p.CycleEnd != "2026-10-31 23:59:59" || p.UsableUntil != time.UnixMilli(2049587698000).UTC().Format(time.RFC3339) || p.ExpiredTime != "" {
		t.Fatalf("package=%+v", p)
	}
	encoded, _ := json.Marshal(result)
	for _, secret := range []string{"private-id", "private-token", "BindRecords", "9999-99"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("unexpected raw metadata %q", secret)
		}
	}
}

func TestPackageNormalization(t *testing.T) {
	for _, tc := range []struct {
		name, raw, total, used, remain, basis string
		precise                               bool
	}{
		{"legacy", `{"CapacitySize":30,"CapacityRemain":16,"CapacityUsed":14}`, "30", "14", "16", "package", false},
		{"derive precise used", `{"CapacitySize":30,"CapacityRemain":16,"CapacityUsed":13,"CapacitySizePrecise":"30","CapacityRemainPrecise":"16.49"}`, "30", "13.51", "16.49", "package", true},
		{"exhausted cycle", `{"CycleCapacitySizePrecise":"100","CycleCapacityRemainPrecise":"0","CapacitySize":999,"CapacityRemain":999}`, "100", "100", "0", "cycle", true},
		{"empty cycle fallback", `{"CycleCapacitySize":0,"CycleCapacityRemain":0,"CapacitySize":30,"CapacityRemain":20}`, "30", "10", "20", "package", false},
		{"zero cycle", `{"CycleCapacitySize":0,"CycleCapacityRemain":0}`, "0", "0", "0", "cycle", false},
		{"tiny fraction", `{"CapacitySizePrecise":"1","CapacityRemainPrecise":"0.000000000000000001"}`, "1", "0.999999999999999999", "0.000000000000000001", "package", true},
		{"overdraw", `{"CapacitySize":30,"CapacityRemain":-1,"CapacityUsed":31}`, "30", "31", "0", "package", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw resourcePackage
			if err := json.Unmarshal([]byte(tc.raw), &raw); err != nil {
				t.Fatal(err)
			}
			p, _, err := normalizePackage(raw)
			if err != nil {
				t.Fatal(err)
			}
			if p.PackageAmounts != (PackageAmounts{Total: tc.total, Used: tc.used, Remaining: tc.remain}) || p.Basis != tc.basis || p.Precise != tc.precise {
				t.Fatalf("package=%+v", p)
			}
		})
	}
}

func TestAccountPackagesMalformedAndEmpty(t *testing.T) {
	for _, raw := range []string{
		`{}`, `{"Response":{}}`, `{"Response":{"Data":{}}}`,
		`{"Response":{"Data":{"TotalCount":-1,"Accounts":[]}}}`,
		`{"Response":{"Data":{"TotalCount":10001,"Accounts":[]}}}`,
		`{"Response":{"Data":{"TotalCount":1,"Accounts":[]}}}`,
		`{"Response":{"Data":{"TotalCount":1,"Accounts":[{}]}}}`,
		`{"Response":{"Data":{"TotalCount":1,"Accounts":[{"CapacitySize":30,"CapacityRemain":16,"CapacityRemainPrecise":"NaN"}]}}}`,
		`{"Response":{"Data":{"TotalCount":1,"Accounts":[{"CapacitySize":30,"CapacityRemain":16,"CapacitySizePrecise":"1e900"}]}}}`,
	} {
		client := packageClient(func(*http.Request) (*http.Response, error) {
			return packageResponse(`{"code":0,"data":` + raw + `}`), nil
		})
		if result, err := client.AccountPackagesContext(context.Background(), &auth.Auth{}); err == nil || result != nil {
			t.Fatalf("malformed response accepted: %s", raw)
		}
	}
	client := packageClient(func(*http.Request) (*http.Response, error) {
		return packageResponse(`{"code":0,"data":{"Response":{"Data":{"TotalCount":0,"Accounts":[]}}}}`), nil
	})
	result, err := client.AccountPackagesContext(context.Background(), &auth.Auth{})
	if err != nil || result.Count != 0 || result.Packages == nil || result.Totals == nil {
		t.Fatalf("empty: %+v %v", result, err)
	}
}

func TestAccountPackagesCancellationAndUpstreamErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := packageClient(func(*http.Request) (*http.Response, error) { t.Fatal("request after cancellation"); return nil, nil })
	if _, err := client.AccountPackagesContext(ctx, &auth.Auth{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
	client = packageClient(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := client.AccountPackagesContext(ctx, &auth.Auth{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout=%v", err)
	}
	client = packageClient(func(*http.Request) (*http.Response, error) {
		return packageResponse(`{"code":12153,"msg":"offline session"}`), nil
	})
	if result, err := client.AccountPackagesContext(context.Background(), &auth.Auth{}); err == nil || result != nil {
		t.Fatalf("business error accepted")
	}
}

func TestAccountPackagesSeparateUnits(t *testing.T) {
	client := packageClient(func(*http.Request) (*http.Response, error) {
		return packageResponse(`{"code":0,"data":{"Response":{"Data":{"TotalCount":2,"Accounts":[{"CapacityUnit":"credits","CapacitySize":30,"CapacityRemain":20},{"CapacityUnit":"requests","CapacitySize":100,"CapacityRemain":40}]}}}}`), nil
	})
	result, err := client.AccountPackagesContext(context.Background(), &auth.Auth{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Totals) != 2 || result.Totals[0].Unit != "credits" || result.Totals[0].Remaining != "20" || result.Totals[1].Remaining != "40" {
		t.Fatalf("mixed unit totals: %+v", result.Totals)
	}
}

func TestAccountPackagesCreditUnitAliases(t *testing.T) {
	client := packageClient(func(*http.Request) (*http.Response, error) {
		return packageResponse(`{"code":0,"data":{"Response":{"Data":{"TotalCount":2,"Accounts":[{"CapacityUnit":"credit","CapacitySizePrecise":"30","CapacityRemainPrecise":"16.49"},{"CapacityUnit":"credits","CapacitySizePrecise":"100","CapacityRemainPrecise":"100"}]}}}}`), nil
	})
	result, err := client.AccountPackagesContext(context.Background(), &auth.Auth{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Totals) != 1 || result.Totals[0].Unit != "credits" || result.Totals[0].Remaining != "116.49" || result.Totals[0].Used != "13.51" || result.Totals[0].Total != "130" {
		t.Fatalf("credit aliases were not combined: %+v", result.Totals)
	}
}
