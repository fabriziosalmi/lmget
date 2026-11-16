package main

import (
	"strings"
	"testing"
	"time"
)

func TestExpiryWarning(t *testing.T) {
	now := time.Date(2026, 12, 10, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		expires time.Time
		want    string
	}{
		{now.Add(8 * 24 * time.Hour), ""},
		{now.Add(7*24*time.Hour + time.Minute), ""},
		{now.Add(5*24*time.Hour + time.Hour), "(in 5 days)"},
		{now.Add(30 * time.Hour), "(in 30 hours)"},
		{now.Add(72*time.Hour - time.Second), "(in 3 days)"},
		{now.Add(-time.Hour), "expired on 2026-12-10"},
	}
	for _, c := range cases {
		got := expiryWarning(c.expires, now)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("expiryWarning(%s) = %q; want %q", c.expires.Sub(now), got, c.want)
		}
	}
}
