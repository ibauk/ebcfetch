package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

func calcClaimDate(hh, mm int, rfc822date time.Time) time.Time {
	/*
	 * calcClaimDate:
	 * The subject line of the email contains a timestamp reflecting the time of day only
	 * I turn this into a fully specified timestamp by reference to other variables
	 * taken from the email and the rally specification.
	 *
	 * The time of day as expressed in the Subject line is treated as being in the rally's timezone.
	 *
	 * If the rally spans a single day, that day is applied. If not then the date is
	 * derived from the email's Date field unless that field refers to an earlier time
	 * of day in which case we use the previous day.
	 *
	 */

	const ymdOnly = "2006-01-02"

	var year, day int
	var mth time.Month
	if cfg.RallyStart.Format(ymdOnly) == cfg.RallyFinish.Format(ymdOnly) {
		year, mth, day = cfg.RallyStart.Date() // Timezone is rally timezone
		if cfg.DebugVerbose {
			fmt.Printf("%v == %v \n", cfg.RallyStart, cfg.RallyFinish)
		}
	} else {
		year, mth, day = rfc822date.In(cfg.LocalTZ).Date() // The datetime parsed from the Date: field of the email. Timezone is whatever it is.
	}
	//	if cfg.DebugVerbose {
	//		fmt.Printf("calcClaimDate called with y=%v m=%v d=%v hh=%v mm=%v\n", year, mth, day, hh, mm)
	//	}
	cd := time.Date(year, mth, day, hh, mm, 0, 0, cfg.LocalTZ)
	hrs := cd.Sub(rfc822date).Hours()
	if hrs > 1 && cd.Day() != cfg.RallyStart.Day() { // Claimed time is more than one hour later than the send (Date:) time of the email
		cd = cd.AddDate(0, 0, -1)
	}
	return cd
}

func calcOffsetString(t time.Time) string {

	_, secs := t.Zone()
	if secs == 0 {
		return "+0000"
	}
	hrs := secs / 60 / 60
	var res string = "+"
	if hrs < 0 {
		res = "-"
		if hrs > -10 {
			res = res + "0"
		}
		hrs = 0 - hrs
	} else {
		if hrs < 10 {
			res = res + "0"
		}
	}
	res = res + strconv.Itoa(hrs) + ":00"
	return res

}

// extractDateOfResentClaim
//
// I examine the existing claims in the database to see whether the current claim is a simple
// retransmit of an earlier claim rather than a new claim.
// If this is a retransmit, I return the same datetime as the original claim otherwise
// I return false and let the normal processes proceed.
//

func extractDateOfResentClaim(EntrantID int, BonusID string, OdoReading int, TimeHH int, TimeMM int) (time.Time, bool) {

	var res time.Time
	var ok bool

	sqlx := "SELECT ClaimTime FROM ebclaims WHERE "
	sqlx += fmt.Sprintf("EntrantID=%v AND BonusID='%v' AND OdoReading=%v AND ClaimHH=%v AND ClaimMM=%v", EntrantID, BonusID, OdoReading, TimeHH, TimeMM)
	sqlx += " ORDER BY ClaimTime,DateTime"
	rows, err := dbh.Query(sqlx)
	if err != nil {
		fmt.Println(sqlx)
		panic(err)
	}
	defer rows.Close()
	if rows.Next() {
		ct := ""
		rows.Scan(&ct)
		res, err = time.ParseInLocation(time.RFC3339, ct, cfg.LocalTZ)
		ok = err == nil
	}
	return res, ok

}

func extractTime(s string) string {
	x := strings.Split(s, ";")
	if len(x) < 1 {
		return ""
	}
	return strings.Trim(x[len(x)-1], " ")
}

func parseTime(s string) time.Time {
	//fmt.Printf("Parsing time from [ %v ]\n", s)
	if s == "" {
		return time.Time{}
	}

	formats := []string{
		time.RFC1123Z,
		"Mon, 2 Jan 2006 15:04:05 -0700",
		time.RFC1123Z + " (MST)",
		"Mon, 2 Jan 2006 15:04:05 -0700 (MST)",
		"2006-01-02T15:04",
		"2006-01-02 15:04",
		"2006-01-02",
	}

	for _, format := range formats {
		t, err := time.ParseInLocation(format, s, cfg.LocalTZ)
		if err == nil {
			//fmt.Printf("Found time\n")
			return t
		}
		//fmt.Printf("Err: %v\n", err)
	}

	return time.Time{}
}

func storeTimeDB(t time.Time) string {

	res := t.Local().Format(timefmt)
	return res
}
