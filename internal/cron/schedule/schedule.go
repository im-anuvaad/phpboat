// Package schedule parses the 5-field cron expressions of [[cron]] jobs
// (docs/app-contract.md §3) and computes their next run.
//
// Fields, in order: minute 0-59, hour 0-23, day of month 1-31, month 1-12,
// day of week 0-7 (0 and 7 are Sunday). Each field is a comma-separated list
// of items; an item is *, a number, a-b, */n or a-b/n. Names (jan, mon) and
// macros (@daily) are not supported. Times are UTC, with minute resolution.
//
// As in vixie cron, a day field whose text starts with '*' counts as
// unrestricted. When both day fields are restricted, a day matches if either
// field matches; otherwise both must match.
package schedule

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed expression.
type Schedule struct {
	expr                          string
	minute, hour, dom, month, dow bits
	domStar, dowStar              bool
}

// bits holds one bit per allowed value (values are below 64).
type bits uint64

func (b bits) has(v int) bool { return b&(1<<uint(v)) != 0 }

type field struct {
	name     string
	min, max int
}

var fields = [5]field{
	{"minute", 0, 59},
	{"hour", 0, 23},
	{"day of month", 1, 31},
	{"month", 1, 12},
	{"day of week", 0, 7},
}

// maxLen bounds the expression length before parsing.
const maxLen = 100

// horizonYears is how far Next searches. The Gregorian calendar repeats every
// 400 years, so a schedule with no match inside that span never matches.
const horizonYears = 400

// String returns the expression as written.
func (s Schedule) String() string { return s.expr }

// Parse parses a 5-field expression. It reports every problem it finds in
// the first failing field, and rejects expressions that never match.
func Parse(expr string) (Schedule, error) {
	var s Schedule
	if len(expr) > maxLen {
		return s, fmt.Errorf("schedule is longer than %d characters", maxLen)
	}
	trimmed := strings.TrimSpace(expr)
	if strings.HasPrefix(trimmed, "@") {
		return s, errors.New(`macros such as @daily are not supported: write the five fields, e.g. "0 0 * * *"`)
	}
	parts := strings.Fields(trimmed)
	if len(parts) != 5 {
		return s, fmt.Errorf("needs 5 fields (minute hour day-of-month month day-of-week), got %d", len(parts))
	}
	var sets [5]bits
	var msgs []string
	for i, p := range parts {
		b, err := parseField(fields[i], p)
		if err != nil {
			msgs = append(msgs, fmt.Sprintf("%s %q: %v", fields[i].name, p, err))
			continue
		}
		sets[i] = b
	}
	if len(msgs) > 0 {
		return s, errors.New(strings.Join(msgs, "; "))
	}
	// 7 is Sunday as well as 0.
	if sets[4].has(7) {
		sets[4] = sets[4]&^(1<<7) | 1
	}
	s = Schedule{
		expr:    trimmed,
		minute:  sets[0],
		hour:    sets[1],
		dom:     sets[2],
		month:   sets[3],
		dow:     sets[4],
		domStar: strings.HasPrefix(parts[2], "*"),
		dowStar: strings.HasPrefix(parts[4], "*"),
	}
	if s.Next(time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)).IsZero() {
		return s, errors.New("the schedule never matches a date (for example day 31 of February)")
	}
	return s, nil
}

func parseField(f field, text string) (bits, error) {
	var b bits
	for _, item := range strings.Split(text, ",") {
		if err := addItem(&b, f, item); err != nil {
			return 0, err
		}
	}
	return b, nil
}

func addItem(b *bits, f field, item string) error {
	if item == "" {
		return errors.New("empty item in the list")
	}
	rng, step, hasStep := strings.Cut(item, "/")
	n := 1
	if hasStep {
		v, err := number(step)
		if err != nil {
			return fmt.Errorf("step %q: %w", step, err)
		}
		if v < 1 || v > f.max-f.min+1 {
			return fmt.Errorf("step %d must be from 1 to %d", v, f.max-f.min+1)
		}
		n = v
	}
	var lo, hi int
	var err error
	switch {
	case rng == "*":
		lo, hi = f.min, f.max
	case strings.Contains(rng, "-"):
		a, z, _ := strings.Cut(rng, "-")
		if a == "" || z == "" {
			return fmt.Errorf("range %s needs a number on each side of '-'", rng)
		}
		if lo, err = value(f, a); err != nil {
			return err
		}
		if hi, err = value(f, z); err != nil {
			return err
		}
		if lo > hi {
			return fmt.Errorf("range %s goes backwards: write the smaller number first", rng)
		}
	case hasStep:
		return fmt.Errorf("a step needs a range: write */%s or a-b/%s", step, step)
	default:
		if lo, err = value(f, rng); err != nil {
			return err
		}
		hi = lo
	}
	for v := lo; v <= hi; v += n {
		*b |= 1 << uint(v)
	}
	return nil
}

// value parses one number of a field, rejecting names and other syntax.
func value(f field, s string) (int, error) {
	if strings.ContainsAny(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") {
		return 0, fmt.Errorf("%q: names such as jan or mon are not supported, use numbers", s)
	}
	v, err := number(s)
	if err != nil {
		return 0, err
	}
	if v < f.min || v > f.max {
		return 0, fmt.Errorf("%d is out of range %d-%d", v, f.min, f.max)
	}
	return v, nil
}

// number parses an unsigned decimal of at most 3 digits.
func number(s string) (int, error) {
	if s == "" {
		return 0, errors.New("missing number")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("unsupported character %q (supported: * numbers , - /)", r)
		}
	}
	if len(s) > 3 {
		return 0, fmt.Errorf("%s is too large", s)
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	return v, nil
}

// dayOK reports whether the day of t matches the day fields.
func (s Schedule) dayOK(t time.Time) bool {
	domOK := s.dom.has(t.Day())
	dowOK := s.dow.has(int(t.Weekday()))
	if s.domStar || s.dowStar {
		return domOK && dowOK
	}
	return domOK || dowOK
}

// Matches reports whether the minute of t (UTC) is a run time.
func (s Schedule) Matches(t time.Time) bool {
	t = t.UTC()
	return s.minute.has(t.Minute()) && s.hour.has(t.Hour()) && s.month.has(int(t.Month())) && s.dayOK(t)
}

// Next returns the first run time strictly after the given instant, in UTC.
// It returns the zero time if there is none within the horizon.
func (s Schedule) Next(after time.Time) time.Time {
	t := after.UTC().Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(horizonYears, 0, 0)
	for t.Before(limit) {
		switch {
		case !s.month.has(int(t.Month())):
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		case !s.dayOK(t):
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.UTC)
		case !s.hour.has(t.Hour()):
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, time.UTC)
		case !s.minute.has(t.Minute()):
			t = t.Add(time.Minute)
		default:
			return t
		}
	}
	return time.Time{}
}
