package main

import (
	"testing"
)

type SUBJECT struct {
	x  string
	ok bool
}

var subjects = []SUBJECT{
	{"1 2 3 1234", true},
	{"01 bb 12345 930", true},
	{"01 02", false},
	{"bob 02 1234 1234", true},
	{"bob bob 1234 12:34", true},
	{"1 Ba,1234,12:34", false},
	{"a01 bac 12345 3.17", true},
	{"A1 BB1 123.456 0440", false},
	{"1A BB1 123456 0440", true},
	{"1A BB1 123456 20099", false},
	{"01 13 2345 1712 bollox and stuff", true},
	{"Fwd: 1 23b 27 1234", true},
	{"Fwd: 1 23b 27 1234 some old bollox", true},
	{"Fwd: 1 23b 27 2023-02-01T07:15:00+03:00 some old bollox", true},
}

var _ = func() bool {
	testing.Init()
	return true
}()

/*
 *
 * No longer care about 'strict', only allowable
 *
func TestStrictSubject(t *testing.T) {
	for _, x := range subjects {
		ff := *parseSubject(x.x, true)
		if ff.ok != x.strict {
			t.Fatalf("Subject %v [%v] returned [%v] rider=%v\n", x.x, x.strict, ff.ok, ff.EntrantID)
		}
	}
}
*/

func TestAllowableSubject(t *testing.T) {
	for _, x := range subjects {
		ff := *parseSubject(x.x, false)
		if ff.ok != x.ok {
			t.Fatalf("Subject %v [%v] returned [%v] rider=%v\n", x.x, x.ok, ff.ok, ff.EntrantID)
		}
	}
}
