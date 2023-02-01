package main

import (
	"testing"
)

type SUBJECT struct {
	x      string
	ok     bool
	strict bool
}

var subjects = []SUBJECT{
	{"1 2 3 1234", true, true},
	{"01 bb 12345 930", true, false},
	{"01 02", false, false},
	{"bob 02 1234 1234", true, false},
	{"bob bob 1234 12:34", true, false},
	{"1 Ba,1234,12:34", false, false},
	{"a01 bac 12345 3.17", true, false},
	{"A1 BB1 123.456 0440", false, false},
	{"1A BB1 123456 0440", true, false},
	{"01 13 2345 1712 bollox and stuff", true, true},
	{"Fwd: 1 23b 27 1234",true,false},
	{"Fwd: 1 23b 27 1234 some old bollox",true,false},
	{"Fwd: 1 23b 27 2023-02-01T07:15:00+03:00 some old bollox",true,false},
}

var _ = func() bool {
	testing.Init()
	return true
}()

func TestStrictSubject(t *testing.T) {
	for _, x := range subjects {
		ff := *parseSubject(x.x, true)
		if ff.ok != x.strict {
			t.Fatalf("Subject %v [%v] returned [%v] rider=%v\n", x.x, x.strict, ff.ok, ff.EntrantID)
		}
	}
}

func TestAllowableSubject(t *testing.T) {
	for _, x := range subjects {
		ff := *parseSubject(x.x, false)
		if ff.ok != x.ok {
			t.Fatalf("Subject %v [%v] returned [%v] rider=%v\n", x.x, x.ok, ff.ok, ff.EntrantID)
		}
	}
}
