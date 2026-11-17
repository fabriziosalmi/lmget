package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// mirror takes no arguments: "-h" or a typo prints the usage and touches
// neither the network nor the store (it once started a real mirror).
func TestMirrorWithAnArgumentDoesNothing(t *testing.T) {
	// any connection counts: a raw TCP listener behind an https URL
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var hits atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			hits.Add(1)
			conn.Close()
		}
	}()
	home := t.TempDir()
	t.Setenv("LMGET_HOME", home)
	t.Setenv("LMGET_INDEX_URL", "https://"+ln.Addr().String()+"/index.json")
	for _, args := range [][]string{{"mirror", "-h"}, {"mirror", "--help"}, {"mirror", "now"}} {
		if code := run(args); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if n := hits.Load(); n > 0 {
		t.Fatalf("%d connections to the index server", n)
	}
	if ents, _ := os.ReadDir(filepath.Join(home, "signed-index")); len(ents) > 0 {
		t.Fatal("the store was written")
	}
}
