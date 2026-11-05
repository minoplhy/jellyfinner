package cloud

import "time"

// BilledSteps is how many billing steps a server has paid for between created
// and end: at least one, with any part of a step counted whole.
func BilledSteps(created, end time.Time, step time.Duration) int64 {
	if step <= 0 {
		step = time.Hour
	}
	d := end.Sub(created)
	if d <= 0 {
		return 1
	}
	n := int64(d / step)
	if d%step != 0 {
		n++
	}
	return n
}

// PaidUntil is the end of the billing step a server is in at now, so keeping
// it until then costs nothing more.
func PaidUntil(created, now time.Time, step time.Duration) time.Time {
	if step <= 0 {
		step = time.Hour
	}
	return created.Add(time.Duration(BilledSteps(created, now, step)) * step)
}

// Cost estimates what a server cost between created and end.
func Cost(created, end time.Time, step time.Duration, hourly float64) float64 {
	return float64(BilledSteps(created, end, step)) * step.Hours() * hourly
}

// HoursStarted counts the billed hours of a server whose billing step began in
// [from, to). A step is paid when it starts, so this is what a day was charged.
func HoursStarted(created, end time.Time, step time.Duration, from, to time.Time) float64 {
	if step <= 0 {
		step = time.Hour
	}
	steps := BilledSteps(created, end, step)
	var hours float64
	for i := int64(0); i < steps; i++ {
		start := created.Add(time.Duration(i) * step)
		if !start.Before(from) && start.Before(to) {
			hours += step.Hours()
		}
	}
	return hours
}
