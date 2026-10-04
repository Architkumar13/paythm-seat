package store

import "testing"

func TestRequestHashIgnoresOrder(t *testing.T) {
	a := RequestHash("show", []string{"A12", "A1"})
	b := RequestHash("show", []string{"A1", "A12"})
	if a != b {
		t.Fatalf("order changed hash: %s vs %s", a, b)
	}
	c := RequestHash("show", []string{"A1", "A13"})
	if a == c {
		t.Fatal("different seats produced the same hash")
	}
	d := RequestHash("other", []string{"A1", "A12"})
	if a == d {
		t.Fatal("different show produced the same hash")
	}
}

func TestNormalizeSeatsRejectsDuplicates(t *testing.T) {
	if _, err := normalizeSeatList([]string{"A1", "A1"}, 10); err == nil {
		t.Fatal("expected duplicate seat to fail")
	}
	got, err := normalizeSeatList([]string{"A2", "A1"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "A2" || got[1] != "A1" {
		t.Fatalf("order not preserved: %#v", got)
	}
}
