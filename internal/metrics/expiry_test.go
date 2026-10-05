package metrics

import (
	"testing"
	"time"
)

func TestSummaryFloorUsesUTCReceiptDays(t *testing.T) {
	now := time.Date(2026, 11, 1, 1, 30, 0, 0, time.FixedZone("fixture", -5*3600))
	got, err := SummaryFloor(now, 90)
	want := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -89)
	if err != nil || !got.Equal(want) {
		t.Fatal("non-UTC summary floor", got, err)
	}
	for _, days := range []int{0, 7, 366} {
		if _, err := SummaryFloor(now, days); err == nil {
			t.Fatal("unsupported summary window")
		}
	}
	if _, err := SummaryFloor(time.Time{}, 90); err == nil {
		t.Fatal("zero clock accepted")
	}
}
