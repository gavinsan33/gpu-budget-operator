package controllers

import (
	"time"

	gpuquotav1alpha1 "github.com/gsanders/gpu-quota-operator/v1alpha1"
)

// periodStart returns the start of the calendar period containing now, for
// the given BudgetPeriod. Periods are calendar-aligned in UTC, not rolling
// windows - Monthly always starts on the 1st, Weekly always starts Monday,
// Daily always starts at midnight, regardless of where "now" falls within
// that period (never "30 days before now").
func periodStart(period gpuquotav1alpha1.BudgetPeriod, now time.Time) time.Time {
	now = now.UTC()
	switch period {
	case gpuquotav1alpha1.PeriodDaily:
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	case gpuquotav1alpha1.PeriodWeekly:
		day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		// time.Weekday: Sunday=0 ... Saturday=6. Days since the most recent Monday.
		daysSinceMonday := (int(day.Weekday()) + 6) % 7
		return day.AddDate(0, 0, -daysSinceMonday)
	case gpuquotav1alpha1.PeriodMonthly:
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	default:
		// Spec validation (+kubebuilder:validation:Enum) requires Period to be
		// one of the three above; unreachable in practice, but default to
		// Monthly rather than panic.
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
}
