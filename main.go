/*
 * I B A U K   -   S C O R E M A S T E R
 *
 * I fetch bonus claims from email and store in ScoreMaster database
 *
 * I am written for readability rather than efficiency, please keep me that way.
 *
 *
 * Copyright (c) 2025 Bob Stammers
 *
 *
 * This file is part of IBAUK-SCOREMASTER.
 *
 * IBAUK-SCOREMASTER is free software: you can redistribute it and/or modify
 * it under the terms of the MIT License
 *
 * IBAUK-SCOREMASTER is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * MIT License for more details.
 *
 */

package main

import (
	"database/sql"
	"flag"
	"fmt"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	_ "github.com/mattn/go-sqlite3"
)

const copyrite = "Copyright (c) 2025 Bob Stammers"

const progdesc = `
I extract Electronic Bonus Claims from the designated email account using IMAP
and load them into the scoring database ready for judging by a human being.

I parse the Subject line for the four fields entrant, bonus, odo and time and 
load any photos into the database. If the Subject line doesn't parse 
correctly, or if the entrant can't be identified, I "unsee" the email and 
don't record it in the database. Such unseen emails must be processed by hand. 
Look for unread emails in a Gmail window.  Photos are expected to be JPGs but 
HEICs are also accepted and can be automatically converted to JPG using an 
external utility.

If using Gmail, 2-factor authentication must be enabled and an 'app password'
must be created. To do that, edit the Google Account settings, [Security].
Check that all is ok before the rally.`

var verbose = flag.Bool("v", false, "Verbose")
var silent = flag.Bool("s", false, "Silent")
var yml = flag.String("cfg", "", "Path of YAML config file")
var showusage = flag.Bool("?", false, "Show this help text")
var path2db = flag.String("db", "ScoreMaster.db", "Path of ScoreMaster database")
var debugwait = flag.Bool("dw", false, "Wait for [Enter] at exit (debug)")
var trapmails = flag.String("trap", "", "Path used to record trapped emails (overrides config)")
var usingchasm = flag.Bool("sm4", false, "Processor is Chasm not SM3")

const apptitle = "EBCFetch"
const appversion = "1.12"
const timefmt = time.RFC3339

var dbh *sql.DB

func main() {

	monitoring := monitoringOK()
	testmode := cfg.TestMode

	showMonitorStatus(monitoring)

	for {
		if monitoring {
			dflags, sflags := fetchNewClaims()
			if *verbose && (dflags != nil || sflags != nil) {
				fmt.Printf("%s dflags=%v; sflags=%v\n", logts(), dflags, sflags)
			}
			safelyFlagSkippedEmails(dflags, true)
			safelyFlagSkippedEmails(sflags, false)
		}

		time.Sleep(time.Duration(cfg.SleepSeconds) * time.Second)

		if ReloadConfigFromDB {
			refreshConfig()
			newmon := monitoringOK()
			if newmon != monitoring || testmode != cfg.TestMode {
				monitoring = newmon
				testmode = cfg.TestMode
				showMonitorStatus(monitoring)
			}
		}
	}
}

// Alphabetical below here

func extractEntrantID(x string) int {

	// Strip any leading non-digits. Trailing non-digits ignored.
	// This made necessary because some event organisers like to decorate rider numbers, no names, no pack drill.
	re := regexp.MustCompile(`[^\d]*(\d+)`)
	en := re.FindStringSubmatch(x)
	if len(en) > 0 {
		res, _ := strconv.Atoi(en[1])
		return res
	}
	return 0

}

func fetchBonus(b string, t string) (string, int) {

	rows, err := dbh.Query("SELECT BriefDesc,Points FROM "+t+" WHERE BonusID=?", b)
	if err != nil {
		fmt.Printf("%s Bonus! %v %v\n", logts(), b, err)
		return "", 0
	}
	defer rows.Close()
	if !rows.Next() {
		return "", 0
	}

	var BriefDesc string
	var Points int
	rows.Scan(&BriefDesc, &Points)
	return BriefDesc, Points

}

func fetchTeamID(eid int) int {

	rows, err := dbh.Query("SELECT TeamID FROM entrants WHERE EntrantID=?", eid)
	if err != nil {
		fmt.Printf("%v can't fetch TeamID\n", logts())
		return 0
	}
	defer rows.Close()
	if !rows.Next() {
		return 0
	}
	var res int
	rows.Scan(&res)
	return res
}

// returns an array of email addresses for all entrants
func listValidTestAddresses() []string {

	sqlx := "SELECT Email FROM entrants"
	rows, err := dbh.Query(sqlx)
	if err != nil {
		panic(err)
	}
	defer rows.Close()
	var res []string
	var email string
	for rows.Next() {
		rows.Scan(&email)
		if email != "" {
			res = append(res, email)
		}
	}
	return res

}

func showMonitorStatus(monitoring bool) {
	if !*silent {
		if !monitoring {
			fmt.Printf("%v %v: Monitoring suspended\n", apptitle, appversion)
		} else {
			fmt.Printf("%v %v: Monitoring %v for %v", apptitle, appversion, cfg.ImapLogin, cfg.RallyTitle)
			if cfg.TestMode {
				fmt.Print(" [ TEST MODE ]")
			}
			fmt.Println()
		}
	}

}

func validateBonus(f4 fourFields) string {

	// We actually don't care about the points so drop them

	res, _ := fetchBonus(f4.BonusID, "bonuses")
	return res

}

// SQL for safely retrieving full names
const RiderNameSQL = "ifnull(entrants.RiderName,ifnull(entrants.RiderFirst,'') || ' ' || ifnull(entrants.RiderLast,'')) AS RiderName"

func validateEntrant(f4 fourFields, from string) (bool, bool) {

	var allE []string
	if cfg.TestMode && !cfg.MatchEmail {
		allE = listValidTestAddresses()
	}

	sqlx := "SELECT " + RiderNameSQL + ",Email,TeamID FROM entrants WHERE EntrantID=" + strconv.Itoa(f4.EntrantID)
	team := fetchTeamID(f4.EntrantID)
	if team > 0 {
		sqlx += " OR TeamID=" + strconv.Itoa(team)
	}
	rows, err := dbh.Query(sqlx)
	if err != nil {
		fmt.Printf("%v Entrant! %v %v\n", logts(), f4.EntrantID, err)
		return false, false
	}
	defer rows.Close()
	if !rows.Next() {
		if *verbose {
			fmt.Printf("%v No such entrant %v\n", logts(), f4.EntrantID)
		}
		return false, false
	}

	var RiderName, Email string
	var TeamID int
	rows.Scan(&RiderName, &Email, &TeamID)
	for rows.Next() {
		var rn, em string
		var tn int
		rows.Scan(&rn, &em, &tn)
		Email += "," + em
	}
	v, _ := mail.ParseAddress(from)      // where the email is sent from
	e, _ := mail.ParseAddressList(Email) // addresses known for this entrant
	ok := !cfg.MatchEmail

	// Email matching options
	//
	// In a live rally, MatchEmail=true means from must match entrant's address,
	//                  MatchEmail=false means don't care
	//
	// In Test mode, MatchEmail=true means return ok if from must match entrant's address,
	//               MatchEmail=false means return ok if from matches any address in database

	if !ok || cfg.TestMode {
		if cfg.TestMode {
			ok = false
		}
		var myE []string
		if cfg.TestMode && !cfg.MatchEmail {
			myE = allE
		} else {
			for _, em := range e {
				myE = append(myE, em.Address)
			}
		}
		for _, em := range myE {
			if *verbose {
				fmt.Printf("%v comparing %v with %v\n", logts(), v.Address, em)
			}
			ok = ok || strings.EqualFold(v.Address, cfg.ImapLogin) // Anything sent from my email address is ok by definition
			ok = ok || strings.EqualFold(em, v.Address)
			if !ok {
				f := func(c rune) bool {
					return c == '@'
				}
				a1 := strings.FieldsFunc(em, f)
				a2 := strings.FieldsFunc(v.Address, f)
				ok = strings.EqualFold(a1[0], a2[0]) // Compare only the 'account' port of the address
				if ok && !*silent {
					fmt.Printf("%v matched email from %v for rider %v <%v> [%v]\n", logts(), v.Address, RiderName, Email, ok)
				}
			}
			if ok {
				break
			}
		}
		if !ok && !*silent {
			fmt.Printf("%v received from %v for rider %v <%v> [%v]\n", logts(), v.Address, RiderName, Email, ok)
		}
	}
	return true, ok && !strings.EqualFold(RiderName, "")
}
