package ui

import "testing"

func TestNfilesSingular(t *testing.T) {
	for n, want := range map[int]string{0: "0 pickle files", 1: "1 pickle file", 2: "2 pickle files"} {
		if got := nfiles(n, "pickle "); got != want {
			t.Errorf("nfiles(%d) = %q; want %q", n, got, want)
		}
	}
	if got := nfiles(1, ""); got != "1 file" {
		t.Errorf("nfiles(1, \"\") = %q", got)
	}
}
