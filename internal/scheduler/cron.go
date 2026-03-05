package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron is a compiled 5-field cron matcher.
type Cron struct {
	minutes field
	hours   field
	dom     field
	months  field
	dow     field
}

type field struct {
	any    bool
	values map[int]struct{}
}

// Parse compiles a standard 5-field cron expression into a matcher.
func Parse(expr string) (Cron, error) {
	parts := strings.Fields(strings.TrimSpace(expr))
	if len(parts) != 5 {
		return Cron{}, fmt.Errorf("cron expression must have 5 fields")
	}
	minutes, err := parseField(parts[0], 0, 59)
	if err != nil {
		return Cron{}, fmt.Errorf("minute: %w", err)
	}
	hours, err := parseField(parts[1], 0, 23)
	if err != nil {
		return Cron{}, fmt.Errorf("hour: %w", err)
	}
	dom, err := parseField(parts[2], 1, 31)
	if err != nil {
		return Cron{}, fmt.Errorf("day_of_month: %w", err)
	}
	months, err := parseField(parts[3], 1, 12)
	if err != nil {
		return Cron{}, fmt.Errorf("month: %w", err)
	}
	dow, err := parseField(parts[4], 0, 6)
	if err != nil {
		return Cron{}, fmt.Errorf("day_of_week: %w", err)
	}
	return Cron{minutes: minutes, hours: hours, dom: dom, months: months, dow: dow}, nil
}

func (c Cron) Next(after time.Time) time.Time {
	t := after.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(2, 0, 0)
	for !t.After(limit) {
		if c.matches(t) {
			return t
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}
}

func (c Cron) matches(t time.Time) bool {
	return c.minutes.match(t.Minute()) &&
		c.hours.match(t.Hour()) &&
		c.dom.match(t.Day()) &&
		c.months.match(int(t.Month())) &&
		c.dow.match(int(t.Weekday()))
}

func parseField(raw string, min, max int) (field, error) {
	raw = strings.TrimSpace(raw)
	if raw == "*" {
		return field{any: true}, nil
	}
	out := field{values: map[int]struct{}{}}
	for _, token := range strings.Split(raw, ",") {
		token = strings.TrimSpace(token)
		if token == "" {
			return field{}, fmt.Errorf("empty token")
		}
		if strings.HasPrefix(token, "*/") {
			step, err := strconv.Atoi(strings.TrimPrefix(token, "*/"))
			if err != nil || step <= 0 {
				return field{}, fmt.Errorf("invalid step %q", token)
			}
			for i := min; i <= max; i += step {
				out.values[i] = struct{}{}
			}
			continue
		}
		n, err := strconv.Atoi(token)
		if err != nil {
			return field{}, fmt.Errorf("invalid token %q", token)
		}
		if n < min || n > max {
			return field{}, fmt.Errorf("value %d outside %d..%d", n, min, max)
		}
		out.values[n] = struct{}{}
	}
	return out, nil
}

func (f field) match(v int) bool {
	if f.any {
		return true
	}
	_, ok := f.values[v]
	return ok
}
