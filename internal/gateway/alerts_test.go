package gateway

import (
	"slices"
	"testing"

	"github.com/shopspring/decimal"
)

func TestCrossed(t *testing.T) {
	d := decimal.RequireFromString
	for _, tc := range []struct {
		limit, before, after string
		want                 []int
	}{
		{"10", "7", "7.9", nil},
		{"10", "7", "8", []int{80}},
		{"10", "7", "12", []int{80, 100}},
		{"10", "8", "9", nil},
		{"10", "9.5", "10", []int{100}},
		{"10", "10", "11", nil},
		{"0", "0", "1", nil},
	} {
		if got := crossed(d(tc.limit), d(tc.before), d(tc.after)); !slices.Equal(got, tc.want) {
			t.Errorf("crossed(%s, %s, %s) = %v, want %v", tc.limit, tc.before, tc.after, got, tc.want)
		}
	}
	if capStatus(d("8"), d("10")) != "warning" || capStatus(d("1"), d("10")) != "ok" || capStatus(d("10"), d("10")) != "exceeded" || capStatus(d("0"), d("0")) != "exceeded" {
		t.Fatal("capStatus")
	}
}
