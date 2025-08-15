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
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/mail"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	smtp "github.com/xhit/go-simple-mail/v2"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	_ "github.com/mattn/go-sqlite3"
	yaml "gopkg.in/yaml.v2"
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
var path2db = flag.String("db", "sm/ScoreMaster.db", "Path of ScoreMaster database")
var debugwait = flag.Bool("dw", false, "Wait for [Enter] at exit (debug)")
var trapmails = flag.String("trap", "", "Path used to record trapped emails (overrides config)")

const apptitle = "EBCFetch"
const appversion = "1.10"
const timefmt = time.RFC3339

// I'll pass files without this extension to ebcimg for conversion
const standardimageextension = ".jpg"

const ResponseStyleYes = ` font-size: large; color: lightgreen; `
const ResponseStyleNo = ` font-size: x-large; color: red; `
const ResponseStyleLbl = ` text-align: right; padding-right: 1em; `

var dbh *sql.DB

var ReloadConfigFromDB bool

var currentUid uint32 // Used to keep track of email fetch recovery
var LastGoodUid uint32

type EmailSettings struct {
	Port     int    `json:"Port"`
	Host     string `json:"Host"`
	Username string `json:"Username"`
	Password string `json:"Password"`
	CertName string `json:"CertName"`
}

var cfg struct {
	ImapServer            string    `yaml:"imapserver"`
	ImapLogin             string    `yaml:"login"`
	ImapPassword          string    `yaml:"password"`
	NotBefore             time.Time `yaml:"notbefore,omitempty"`
	NotAfter              time.Time `yaml:"notafter,omitempty"`
	Subject               string    `yaml:"subject"`
	Strict                string    `yaml:"strict"`
	SubjectRE             *regexp.Regexp
	StrictRE              *regexp.Regexp
	RallyTitle            string
	RallyStart            time.Time
	RallyFinish           time.Time
	LocalTimezone         string
	LocalTZ               *time.Location
	OffsetTZ              string
	SelectFlags           []string `yaml:"selectflags"`
	CheckStrict           bool     `yaml:"checkstrict"`
	SleepSeconds          int      `yaml:"sleepseconds"`
	Path2SM               string   `yaml:"path2sm"`
	ImageFolder           string   `yaml:"imagefolder"`
	MatchEmail            bool     `yaml:"matchemail"`
	Heic2jpg              string   `yaml:"heic2jpg"`
	ConvertHeic           bool     `yaml:"convertheic2jpg"`
	DontRun               bool     `yaml:"dontrun"`
	KeyWait               bool     `yaml:"debugwait"`
	AllowBody             bool     `yaml:"allowbody"`
	TrapMails             bool     `yaml:"trapmails"`
	TrapPath              string   `yaml:"trappath"`
	TestMode              bool     `yaml:"testmode"`
	SmtpStuff             EmailSettings
	TestModeLiteral       string `yaml:"TestModeLiteral"`
	TestResponseSubject   string `yaml:"TestResponseSubject"`
	TestResponseGood      string `yaml:"TestResponseGood"`
	TestResponseBad       string `yaml:"TestResponseBad"`
	TestResponseAdvice    string `yaml:"TestResponseAdvice"`
	TestResponseBCC       string `yaml:"TestResponseBCC"`
	TestResponseBadEmail  string `yaml:"TestResponseBadEmail"`
	TestResponseGoodEmail string `yaml:"TestResponseGoodEmail"`
	MaxExtraPhotos        int    `yaml:"MaxExtraPhotos"`
	DebugVerbose          bool   `yaml:"verbose"`
	MaxFetch              int    `yaml:"MaxFetch"`
	MaxBacktrack          int    `yaml:"MaxBacktrack"`
}

// fourFields: this contains the results of parsing the Subject line.
// The "four fields" are entrant, bonus, odo & claimtime
type fourFields struct {
	ok         bool
	EntrantID  int
	BonusID    string
	OdoReading int
	OdoOk      bool
	ClaimTime  time.Time
	HHmm       string
	TimeOk     bool
	TimeHH     int
	TimeMM     int
	Extra      string
}

// testResponse contains the response to be sent to the sender when
// running in TestMode. This gives detailed feedback on the test
// email received.
type testResponse struct {
	AddressIsRegistered bool   // Sender's email is registered for rally
	ClaimSubject        string // The claim string retrieved from Subject or body
	SubjectFromBody     bool
	PhotoPresent        int
	EntrantID           int
	ValidEntrantID      bool
	BonusID             string
	BonusIsReal         bool
	BonusDesc           string
	OdoReading          int
	HHmm                string
	ClaimDateTime       time.Time
	ExtraField          string
	Commentary          string
	ClaimIsGood         bool
	ClaimIsPerfect      bool
}

const myTimeFormat = "2006-01-02 15:04:05"

type timestamp struct {
	date time.Time
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
	}

	for _, format := range formats {
		t, err := time.Parse(format, s)
		if err == nil {
			//fmt.Printf("Found time\n")
			return t
		}
		//fmt.Printf("Err: %v\n", err)
	}

	return time.Time{}
}

func timeFromPhoto(fname string, cd string) time.Time {

	//fmt.Printf("tfp: '%v' = '%v'\n", fname, cd)
	// 20210717_185053.jpg
	nametimRE := regexp.MustCompile(`(\d{4})(\d\d)(\d\d)_(\d\d)(\d\d)(\d\d)`)
	ptime := time.Time{}
	xx := nametimRE.FindStringSubmatch(fname)

	//fmt.Printf("tfp1: %v\n", xx)
	if xx == nil {
		//fmt.Printf("Filename %v is not a timestamp\n", fname)
		// modification-date="Sat, 17 Jul 2021 17:57:48 GMT
		modRE := regexp.MustCompile(`modification\-date="([^"]+)"`)
		creRE := regexp.MustCompile(`creation\-date="([^"]+)"`)
		xx = modRE.FindStringSubmatch(cd)
		if xx == nil {
			xx = creRE.FindStringSubmatch(cd)
		}
		if xx != nil {

			ptime, _ = time.Parse(time.RFC1123, xx[1])
		}
	} else {
		//fmt.Printf("%v\n", xx)
		ptime, _ = time.Parse(time.RFC3339, xx[1]+"-"+xx[2]+"-"+xx[3]+"T"+xx[4]+":"+xx[5]+":"+xx[6]+cfg.OffsetTZ)
	}
	return ptime
}

func nameFromContentType(ct string) string {

	re := regexp.MustCompile(`\"(.+)\"`)
	sm := re.FindSubmatch([]byte(ct))
	if len(sm) > 1 {
		return string(sm[1])
	}
	return ct

}

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

func fetchConfigFromDB() (string, []byte) {
	rows, err := dbh.Query("SELECT ebcsettings,EmailParams FROM rallyparams")
	if err != nil {
		fmt.Printf("%s can't fetch config from database [%v] run aborted\n", logts(), err)
		osExit(1)
	}
	defer rows.Close()
	rows.Next()
	var yaml string
	var json []byte
	rows.Scan(&yaml, &json)
	return yaml, json

}
func fetchNewClaims() (*imap.SeqSet, *imap.SeqSet) {

	// Connect to server
	c, err := client.DialTLS(cfg.ImapServer, nil)
	if err != nil {
		log.Printf("DialTLS: %v\n", err)
		return nil, nil
	}

	// Don't forget to logout
	defer c.Logout()

	// Login
	if err := c.Login(cfg.ImapLogin, cfg.ImapPassword); err != nil {
		log.Printf("Login: %v\n", err)
		return nil, nil
	}

	// Select INBOX
	_, err = c.Select("INBOX", false)
	if err != nil {
		log.Printf("Select: %v\n", err)
		return nil, nil
	}
	criteria := imap.NewSearchCriteria()
	criteria.WithoutFlags = cfg.SelectFlags
	nulltime := time.Time{}
	if cfg.NotBefore != nulltime {
		criteria.SentSince = cfg.NotBefore
	}
	if cfg.NotAfter != nulltime {
		criteria.SentBefore = cfg.NotAfter
	}

	//	if *verbose {
	//		fmt.Printf("%s searching ... ", logts())
	//	}
	uids, err := c.Search(criteria)
	if err != nil {
		log.Printf("Search: %v\n", err)
	}
	//	if *verbose {
	//		fmt.Printf("%s ok\n", logts())
	//	}

	// Collect the unique IDs of messages found
	seqset := new(imap.SeqSet)

	Nmax := 0

	if cfg.MaxFetch < 1 || len(uids) <= cfg.MaxFetch { // Unlimited fetch
		seqset.AddNum(uids...)
		Nmax = len(uids)
	} else {
		for i := 0; i < cfg.MaxFetch; i++ {
			seqset.AddNum(uids[i])
			Nmax++
		}
	}

	//seqset.AddNum(uids...)
	if seqset.Empty() { // Didn't find any messages so we're done
		return nil, nil
	}

	if *verbose {
		fmt.Printf("%s fetching %v message(s)\n", logts(), Nmax)
	}

	skipped := new(imap.SeqSet)   // Will contain UIDs of claims to be revisited. Possibly couldn't get DB lock
	dealtwith := new(imap.SeqSet) // Will contain UIDs of non-claims

	N := 0

	// Get the whole message body, automatically sets //Seen
	section := &imap.BodySectionName{}
	items := []imap.FetchItem{section.FetchItem(), imap.FetchUid, imap.FetchInternalDate}
	messages := make(chan *imap.Message, 1)
	done := make(chan error, 1)
	go func() {
		done <- c.Fetch(seqset, items, messages)
	}()

	for msg := range messages {

		currentUid = msg.Uid

		N++
		if *verbose {
			fmt.Printf("Considering msg #%v=%v\n", N, currentUid)
		}

		var TR testResponse

		r := msg.GetBody(section) // This automatically marks the message as 'read'
		if r == nil {
			log.Println("Server didn't return message body")
			continue
		}
		m, err := Parse(r)
		if err != nil {
			log.Printf("Parse error: %v\n", err)
			continue
		}

		if cfg.TrapMails && cfg.TrapPath != "" {
			fmt.Printf("INCOMING: %v\n", m)
		}

		f4 := parseSubject(m.Subject, false)
		if m.Subject == "" && cfg.AllowBody {
			if cfg.DebugVerbose {
				fmt.Println("Parsing body for Subject:")
			}
			f4 = parseSubject(m.TextBody, false)
			if f4.ok {
				m.Subject = m.TextBody
				TR.SubjectFromBody = true
			}
		}

		if *verbose {
			fmt.Printf("%s %v [ %v ]\n", logts(), currentUid, m.Subject)
		}
		TR.ClaimSubject = m.Subject
		TR.EntrantID = f4.EntrantID
		TR.BonusID = f4.BonusID
		TR.OdoReading = f4.OdoReading
		TR.HHmm = f4.HHmm
		if !f4.ClaimTime.IsZero() {
			TR.ClaimDateTime = f4.ClaimTime
		} else {
			ok := false
			TR.ClaimDateTime, ok = extractDateOfResentClaim(f4.EntrantID, f4.BonusID, f4.OdoReading, f4.TimeHH, f4.TimeMM)
			if !ok {
				TR.ClaimDateTime = calcClaimDate(f4.TimeHH, f4.TimeMM, m.Date)
			}
			f4.ClaimTime = TR.ClaimDateTime
		}
		TR.ExtraField = f4.Extra

		ve, vea := validateEntrant(*f4, m.Header.Get("From"))
		TR.ValidEntrantID = ve && f4.EntrantID > 0
		TR.AddressIsRegistered = vea

		if *verbose {
			fmt.Printf("%s ve=%v, vea=%v, Entrant=%v\n", logts(), ve, vea, f4.EntrantID)
		}
		// If ve is false then I don't know who the entrant is so I must not create a claim in ScoreMaster
		// In TestMode we do want to process the email and respond even though ve is false

		vb := validateBonus(*f4)
		TR.BonusIsReal = vb != ""
		TR.BonusDesc = vb

		if *verbose {
			fmt.Printf("%s bonus is %v : %v\n", logts(), TR.BonusIsReal, TR.BonusDesc)
		}

		if !vea && !cfg.TestMode {
			if !*silent {
				okx := "ok"
				if !f4.ok {
					okx = "FALSE"
				}
				vex := "ok"
				if !ve {
					vex = "FALSE"
				}
				vbx := "ok"
				if vb == "" {
					vbx = "FALSE"
				}
				fmt.Printf("%v skipping %v [%v] ok=%v,ve=%v,vb=%v\n", logts(), m.Subject, msg.Uid, okx, vex, vbx)
			}
			dealtwith.AddNum(msg.Uid) // Can't / won't process but don't want to see it again
			if !cfg.TestMode {
				continue
			}
		} else {

			TR.ClaimIsGood = f4.ok && ve && (vea || !cfg.MatchEmail) && vb != "" && f4.TimeOk

		}

		var photoid int = 0
		var photoids string
		var photoTime time.Time
		var numphotos int = 0
		var photosok bool = true
		for _, a := range m.Attachments {
			if *verbose {
				fmt.Printf("%s Att: CD = %v\n", logts(), a.ContentDisposition)
			}
			pt := timeFromPhoto(a.Filename, a.ContentDisposition)
			numphotos++
			if pt.After(photoTime) {
				photoTime = pt
			}
			pix, err := io.ReadAll(a.Data)
			if err != nil {
				if !*silent {
					fmt.Printf("%s attachment error %v\n", logts(), err)
					photosok = false
					break
				}
			} else {
				photoid = writeImage(f4.EntrantID, f4.BonusID, msg.Uid, pix, string(a.Filename))
				if photoid == 0 && !cfg.TestMode {
					photosok = false
					break
				}
				if photoids != "" {
					photoids += ","
				}
				photoids += strconv.Itoa(photoid)
				if *verbose {
					fmt.Printf("%s attachment of size %v bytes\n", logts(), len(pix))
				}
			}
			//fmt.Printf("  Photo: %v\n", pt.Format(myTimeFormat))
		}
		for _, a := range m.EmbeddedFiles {
			if *verbose {
				fmt.Printf("%s Emb: CD = %v\n", logts(), a.ContentDisposition)
			}
			pt := timeFromPhoto(nameFromContentType(a.ContentType), a.ContentDisposition)
			numphotos++
			if pt.After(photoTime) {
				photoTime = pt
			}
			pix, err := io.ReadAll(a.Data)
			if err != nil {
				if !*silent {
					fmt.Printf("%s embedding error %v\n", logts(), err)
					photosok = false
					break
				}
			} else {
				photoid = writeImage(f4.EntrantID, f4.BonusID, msg.Uid, pix, a.ContentDisposition)
				if photoid == 0 && !cfg.TestMode {
					photosok = false
					break
				}
				if photoids != "" {
					photoids += ","
				}
				photoids += strconv.Itoa(photoid)

				if *verbose {
					fmt.Printf("%s embedded image of size %v bytes\n", logts(), len(pix))
				}
			}
			if *verbose {
				fmt.Printf("%s photo: %v\n", logts(), pt.Format(myTimeFormat))
			}
		}

		if photosok {
			TR.PhotoPresent = numphotos
		} else if numphotos > 0 {
			TR.PhotoPresent = 0 - numphotos
		}

		if numphotos != 1 {
			photoid = 0 // Make ScoreMaster hunt for photos
		}

		var sentatTime time.Time = msg.InternalDate
		for _, xr := range m.Header["X-Received"] {
			ts := timestamp{parseTime(extractTime(xr)).Local()}
			if ts.date.Before(sentatTime) {
				sentatTime = ts.date
			}
		}
		for _, xr := range m.Header["Received"] {
			ts := timestamp{parseTime(extractTime(xr)).Local()}
			if ts.date.Before(sentatTime) {
				sentatTime = ts.date
			}
		}

		if cfg.TestMode {
			sendTestResponse(TR, m.Header.Get("From"), f4)
			continue
		} else {

			var sb strings.Builder
			sb.WriteString("INSERT INTO ebclaims (LoggedAt,DateTime,EntrantID,BonusID,OdoReading,")
			sb.WriteString("FinalTime,EmailID,ClaimHH,ClaimMM,ClaimTime,Subject,ExtraField,")
			sb.WriteString("StrictOk,AttachmentTime,FirstTime,PhotoID) ")
			sb.WriteString("VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)")
			_, err = dbh.Exec(sb.String(), storeTimeDB(time.Now()), storeTimeDB(m.Date.Local()),
				f4.EntrantID, f4.BonusID, f4.OdoReading,
				storeTimeDB(msg.InternalDate), msg.Uid, f4.TimeHH, f4.TimeMM,
				//storeTimeDB(calcClaimDate(f4.TimeHH, f4.TimeMM, m.Date)),
				storeTimeDB(f4.ClaimTime),
				m.Subject, f4.Extra,
				false, photoTime, sentatTime, photoids) // Writing photoids NOT photoid
			if err != nil {
				if !*silent {
					fmt.Printf("%s can't store claim - %v\n", logts(), err)
				}
				skipped.AddNum(msg.Uid) // Can't process now but I'll try again later
				continue
			}
			LastGoodUid = msg.Uid

		}
		if !*silent {
			fmt.Printf("Claiming #%v [ %v ]\n", msg.Uid, m.Subject)
		}

	} // End msg loop

	if err := <-done; err != nil {
		if !*silent {
			fmt.Printf("%s OMG!! msg=%v (%v / %v) %v\n", logts(), currentUid, N, Nmax, err)
		}
		for i := uint32(0); i < uint32(cfg.MaxBacktrack); i++ {
			if currentUid+i > LastGoodUid {
				skipped.AddNum(currentUid + i)
			}
		}
		//return
	}

	return dealtwith, skipped

}

func flagSkippedEmails(ss *imap.SeqSet, ignoreThem bool) {

	// Connect to server
	c, err := client.DialTLS(cfg.ImapServer, nil)
	if err != nil {
		log.Printf("fse:DialTLS: %v\n", err)
		return
	}

	// Don't forget to logout
	defer c.Logout()

	// Login
	if err := c.Login(cfg.ImapLogin, cfg.ImapPassword); err != nil {
		log.Printf("fse:Login: %v\n", err)
		return
	}

	// Select INBOX
	_, err = c.Select("INBOX", false)
	if err != nil {
		log.Printf("fse:Select: %v\n", err)
		return
	}

	item := imap.FormatFlagsOp(imap.SetFlags, true)
	flags := []interface{}{}
	if ignoreThem {
		flags = []interface{}{imap.FlaggedFlag}
	}
	if *verbose {
		msg := "releasing for retry"
		if ignoreThem {
			msg = "leaving unread"
		}

		fmt.Printf("%s %s %v %v %v\n", logts(), msg, ss, item, flags)
	}
	err = c.UidStore(ss, item, flags, nil)
	if err != nil {
		log.Println(err)
		return
	}

}

func init() {

	//ex, _ := os.Executable()
	//exPath := filepath.Dir(ex)
	//os.Chdir(exPath)

	flag.Usage = func() {
		w := flag.CommandLine.Output()
		fmt.Fprintf(w, "%v v%v\n", apptitle, appversion)
		flag.PrintDefaults()
		fmt.Fprintf(w, "%v\n", progdesc)
	}
	flag.Parse()
	if *showusage {
		flag.Usage()
		os.Exit(1)
	}

	if !*silent {
		fmt.Printf("%v: v%v   %v\n", apptitle, appversion, copyrite)
	}

	if *path2db == "" {
		fmt.Printf("%s No database has been specified Run aborted\n", apptitle)
		osExit(1)
	}

	openDB(*path2db)

	configPath := *yml

	if strings.EqualFold(configPath, "") {
		configPath = "config"
		refreshConfig()
		ReloadConfigFromDB = true
	} else {
		if _, err := os.Stat(configPath); os.IsNotExist(err) {
			wd, _ := os.Getwd()
			fmt.Printf("%s: Can't access %v [%v], run aborted\n", apptitle, configPath, wd)
		}

		file, err := os.Open(configPath)
		if err == nil {

			defer file.Close()

			D := yaml.NewDecoder(file)
			D.Decode(&cfg)
		}
		cfg.Path2SM = filepath.Dir(*path2db)
	}

	/*
	 * These are now live switcheable options. I'll continue to run but won't do anything unless
	 * this option is reset.
	 *
	 */
	if cfg.DontRun {
		if !*silent {
			fmt.Printf("%s: DontRun option triggered, enough already\n", apptitle)
		}
	}

	/*
	 * This is now a live switcheable option. I'll continue to run but won't do anything unless
	 * this option is reset.
	 *
	 */
	if cfg.ImapServer == "" || cfg.ImapLogin == "" {
		fmt.Printf("%s: Email configuration has not been specified\n", apptitle)
		fmt.Printf("%s: Email fetching will not be possible. Please fix %v and retry\n", apptitle, configPath)
	} else if cfg.ImapPassword == "" {
		fmt.Printf("%s: No password has been set for incoming IMAP account %v\n", apptitle, cfg.ImapServer)
		fmt.Printf("%s: Email fetching will not be possible. Please fix %v and retry\n", apptitle, configPath)
	}

	if *trapmails != "" {
		cfg.TrapPath = *trapmails
		cfg.TrapMails = true
	}

	cfg.StrictRE = regexp.MustCompile(cfg.Strict)
	cfg.SubjectRE = regexp.MustCompile(cfg.Subject)

	if !loadRallyData() {
		fmt.Printf("%s: Email fetching will not be possible. Please fix %v and retry\n", apptitle, configPath)
		osExit(1)
	}
	if cfg.ConvertHeic {
		validateHeicHandler()
	}
}

func loadRallyData() bool {

	rows, err := dbh.Query("SELECT RallyTitle, StartTime as RallyStart,FinishTime as RallyFinish,LocalTZ FROM rallyparams")
	if err != nil {
		fmt.Printf("%s: OMG %v\n", apptitle, err)
		osExit(1)
	}
	defer rows.Close()
	rows.Next()
	var RallyStart, RallyFinish, LocalTZ string
	rows.Scan(&cfg.RallyTitle, &RallyStart, &RallyFinish, &LocalTZ)

	cfg.LocalTimezone = LocalTZ
	cfg.LocalTZ, err = time.LoadLocation(LocalTZ)
	if err != nil {
		fmt.Printf("%s Timezone %s cannot be loaded\n", apptitle, LocalTZ)
		return false
	}

	// Make the rally timezone our timezone, regardless of what the server is set to
	time.Local = cfg.LocalTZ

	cfg.RallyStart, err = time.ParseInLocation("2006-01-02T15:04", RallyStart, cfg.LocalTZ)
	if err != nil {
		fmt.Printf("%s RallyStart %s cannot be parsed\n", apptitle, RallyStart)
		return false
	}
	cfg.OffsetTZ = calcOffsetString(cfg.RallyStart)
	if *verbose {
		fmt.Printf("%s: Rally timezone is %v [%v]\n", apptitle, cfg.LocalTZ, cfg.OffsetTZ)
	}
	cfg.RallyFinish, err = time.ParseInLocation("2006-01-02T15:04", RallyFinish, cfg.LocalTZ)
	if err != nil {
		fmt.Printf("%s RallyFinish %s cannot be parsed\n", apptitle, RallyFinish)
		return false
	}
	return true

}

func logts() string {

	var t = time.Now()
	return t.Format("2006-01-02 15:04:05")

}

// Checks configuration for possibility to monitor emails
func monitoringOK() bool {

	res := !cfg.DontRun && cfg.ImapPassword != "" && cfg.ImapServer != "" && cfg.ImapLogin != ""
	return res

}

func main() {

	monitoring := monitoringOK()
	testmode := cfg.TestMode

	showMonitorStatus(monitoring)

	for {
		if monitoring {
			dflags, sflags := fetchNewClaims()
			if dflags != nil && !dflags.Empty() && !cfg.TestMode {
				flagSkippedEmails(dflags, true)
			}
			if sflags != nil && !sflags.Empty() && !cfg.TestMode {
				flagSkippedEmails(sflags, false)
			}
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

func openDB(dbpath string) {

	var err error
	if _, err = os.Stat(dbpath); errors.Is(err, os.ErrNotExist) {
		fmt.Printf("%v: Cannot access database %v [%v] run aborted\n", apptitle, dbpath, err)
		osExit(1)
	}

	dbh, err = sql.Open("sqlite3", dbpath)
	if err != nil {
		fmt.Printf("%v: Can't access database %v [%v] run aborted\n", apptitle, dbpath, err)
		osExit(1)
	}

}

func osExit(res int) {

	if *debugwait || cfg.KeyWait {
		waitforkey()
	}

	defer os.Exit(res)
	runtime.Goexit()

}

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

func parseSubject(s string, formal bool) *fourFields {

	var f4 fourFields
	var ff []string

	if formal {
		ff = cfg.StrictRE.FindStringSubmatch(s)
	} else {
		ff = cfg.SubjectRE.FindStringSubmatch(s)
	}
	if ff == nil && cfg.DebugVerbose {
		fmt.Printf("Matching %v %v returned nil\n", formal, s)
	}
	f4.ok = len(ff) > 0
	if formal && len(ff) < 5 {
		f4.ok = false
	}
	if !f4.ok {
		return &f4
	}
	f4.EntrantID = extractEntrantID(ff[1])
	f4.BonusID = strings.ToUpper(ff[2])
	if len(ff) < 5 {
		return &f4
	}
	f4.OdoReading, _ = strconv.Atoi(ff[3])
	OdoRE := regexp.MustCompile(`^\d+$`)
	f4.OdoOk = OdoRE.MatchString(ff[3])

	var err error
	f4.ClaimTime, err = time.ParseInLocation(time.RFC3339, ff[4], cfg.LocalTZ)
	if err != nil {
		hmx := strings.ReplaceAll(strings.ReplaceAll(ff[4], ":", ""), ".", "")
		if len(hmx) < 4 {
			hmx = "0" + hmx
		}
		f4.HHmm = hmx
		TimeRE := regexp.MustCompile(`^\d\d\d\d$`)
		hm, _ := strconv.Atoi(hmx)
		f4.TimeHH = hm / 100
		f4.TimeMM = hm % 100
		f4.TimeOk = TimeRE.MatchString(hmx) && f4.TimeHH < 24 && f4.TimeMM < 60
		//fmt.Printf("TimeOk - %v == %v\n", hmx, f4.TimeOk)
	} else {
		f4.HHmm = ff[4]
		f4.TimeHH = f4.ClaimTime.Hour()
		f4.TimeMM = f4.ClaimTime.Minute()
		f4.TimeOk = f4.TimeHH < 24 && f4.TimeMM < 60
	}

	if !f4.TimeOk {
		f4.ok = false
	}

	if len(ff) > 5 {
		f4.Extra = ff[5]
	}

	//	if cfg.DebugVerbose {
	//		fmt.Printf("%v [%v] (%v) '%v' == %v; %v == %v; Odo=%v; Time=%v; Extra='%v'\n", formal, s, len(ff), ff[1], f4.EntrantID, ff[2], f4.BonusID, f4.OdoReading, f4.HHmm, f4.Extra)
	//	}

	return &f4
}

func refreshConfig() {

	ymltext, jsontext := fetchConfigFromDB()
	file := strings.NewReader(ymltext)
	D := yaml.NewDecoder(file)
	D.Decode(&cfg)
	json.Unmarshal(jsontext, &cfg.SmtpStuff)
	if cfg.DebugVerbose {
		*verbose = true
	}
	if cfg.MaxBacktrack < 1 {
		cfg.MaxBacktrack = 3 // Sensible default
	}
	cfg.Path2SM = filepath.Dir(*path2db)
	if *verbose {
		fmt.Printf("%s refreshConfig: MaxFetch=%v, MaxBacktrack=%v\n", logts(), cfg.MaxFetch, cfg.MaxBacktrack)
	}

}

func sendAlertToBob(whatsup string) {

	var sendToAddress = []string{"stammers.bob@gmail.com", "webmaster@ironbutt.co.uk"}
	const alertSubject = "EBCFetch alert"

	//fmt.Printf("WhatsUp: %v\n", whatsup)
	client := smtp.NewSMTPClient()
	client.Host = cfg.SmtpStuff.Host
	client.Port = cfg.SmtpStuff.Port
	client.Username = cfg.SmtpStuff.Username
	client.Password = cfg.SmtpStuff.Password

	//fmt.Printf("U:%v P:%v\n", client.Username, client.Password)
	client.Encryption = smtp.EncryptionTLS // It's 2022, everybody needs TLS now, don't they.

	client.ConnectTimeout = 10 * time.Second
	client.SendTimeout = 10 * time.Second
	client.KeepAlive = false

	if cfg.SmtpStuff.CertName != "" {
		client.TLSConfig = &tls.Config{ServerName: cfg.SmtpStuff.CertName}
	}

	conn, err := client.Connect()
	if err != nil {
		fmt.Printf("Can't connect to %v because %v\n", client.Host, err)
		return
	}
	msg := smtp.NewMSG()
	for _, k := range sendToAddress {
		msg.AddTo(k)
	}
	msg.SetFrom(cfg.ImapLogin)
	msg.SetSubject(alertSubject)

	msg.SetBody(smtp.TextPlain, whatsup)

	msg.Send(conn)
	fmt.Printf("%v sending alert to %v\n", logts(), sendToAddress)

}

// sendTestResponse generates and sends a narrative email to the sender
// of any emails received while cfg.TestMode is true.
func sendTestResponse(tr testResponse, from string, f4 *fourFields) {

	var sb strings.Builder

	maxphoto := 1 + cfg.MaxExtraPhotos

	if tr.ClaimIsGood && tr.PhotoPresent > 0 && tr.PhotoPresent <= maxphoto {
		sb.WriteString("<p>" + cfg.TestResponseGood)
	} else {
		sb.WriteString("<p>" + cfg.TestResponseBad)
	}
	sb.WriteString(" [ " + cfg.RallyTitle + " ")
	sb.WriteString("(TZ=" + cfg.LocalTimezone + " " + cfg.OffsetTZ + ") ")
	sb.WriteString(cfg.TestModeLiteral + " ]</p>")

	sb.WriteString("<table>")
	sb.WriteString(`<tr><td style="` + ResponseStyleLbl + `">Subject</td><td>`)
	sb.WriteString(tr.ClaimSubject)
	if tr.SubjectFromBody {
		sb.WriteString(" &#x2611;")
	}
	sb.WriteString((" " + yesno(cfg.SubjectRE.MatchString(tr.ClaimSubject) && f4.TimeOk)))
	sb.WriteString(`</td></tr><tr><td style="` + ResponseStyleLbl + `">Entrant#</td><td>` + strconv.Itoa(f4.EntrantID))
	sb.WriteString(yesno(tr.ValidEntrantID))
	if tr.ValidEntrantID {
		sb.WriteString(`</td></tr><tr><td style="` + ResponseStyleLbl + `">Email = Entrant Email</td><td>`)
		sb.WriteString(yesno(tr.AddressIsRegistered))
		if !tr.AddressIsRegistered {
			sb.WriteString(" " + cfg.TestResponseBadEmail)
		} else {
			sb.WriteString(" " + cfg.TestResponseGoodEmail)
		}
	}

	sb.WriteString(`</td></tr><tr><td style="` + ResponseStyleLbl + `">Bonus</td><td>`)
	sb.WriteString(tr.BonusID)
	if tr.BonusIsReal {
		sb.WriteString(" - ")
		sb.WriteString(tr.BonusDesc)
		sb.WriteString(yesno(true))
	} else {
		sb.WriteString(yesno(false))
	}
	sb.WriteString(`</td></tr><tr><td style="` + ResponseStyleLbl + `">Odo</td><td>`)
	sb.WriteString(strconv.Itoa(tr.OdoReading) + yesno(f4.OdoOk))
	sb.WriteString(`</td></tr><tr><td style="` + ResponseStyleLbl + `">hhmm '` + tr.HHmm + `'</td><td>`)
	sb.WriteString(yesno(f4.TimeOk))
	sb.WriteString(" " + tr.ClaimDateTime.Format(time.UnixDate))
	sb.WriteString(" / " + tr.ClaimDateTime.Format(time.RFC3339))
	if tr.ExtraField != "" {
		sb.WriteString(`</td></tr><tr><td style="` + ResponseStyleLbl + `">&#x270D;</td><td>`)
		sb.WriteString(tr.ExtraField)
	}
	sb.WriteString(`</td></tr><tr><td style="` + ResponseStyleLbl + `">Photo</td><td>`)

	if tr.PhotoPresent > 1 {
		sb.WriteString(` x ` + strconv.Itoa(tr.PhotoPresent) + " ")
	}

	sb.WriteString(yesno(tr.PhotoPresent > 0 && tr.PhotoPresent <= maxphoto))

	if tr.PhotoPresent > maxphoto {
		sb.WriteString("  (max = " + strconv.Itoa(maxphoto) + ")")
	}
	sb.WriteString("</td></tr></table>")

	if cfg.TestResponseAdvice != "" {
		sb.WriteString("<p>" + cfg.TestResponseAdvice + "</p>")
	}

	sb.WriteString("<p>ScoreMaster [" + apptitle + " v" + appversion + " :]</p>")

	if cfg.SmtpStuff.Password == "" {
		fmt.Println("ERROR: Can't send test response, password is empty")
		return
	}
	client := smtp.NewSMTPClient()
	client.Host = cfg.SmtpStuff.Host
	client.Port = cfg.SmtpStuff.Port
	client.Username = cfg.SmtpStuff.Username
	client.Password = cfg.SmtpStuff.Password

	client.Encryption = smtp.EncryptionTLS // It's 2022, everybody needs TLS now, don't they.

	client.ConnectTimeout = 10 * time.Second
	client.SendTimeout = 10 * time.Second
	client.KeepAlive = false

	if cfg.SmtpStuff.CertName != "" {
		client.TLSConfig = &tls.Config{ServerName: cfg.SmtpStuff.CertName}
	}

	conn, err := client.Connect()
	if err != nil {
		fmt.Printf("Can't connect to %v because %v\n", client.Host, err)
		return
	}
	msg := smtp.NewMSG()
	msg.AddTo(from)
	if cfg.TestResponseBCC != "" {
		msg.AddBcc(cfg.TestResponseBCC)
	}
	msg.SetFrom(cfg.ImapLogin)
	if cfg.TestResponseSubject != "" {
		msg.SetSubject(cfg.TestResponseSubject)
	} else if tr.ClaimIsGood && tr.PhotoPresent > 0 && tr.PhotoPresent <= maxphoto {
		msg.SetSubject("EBC test: " + cfg.TestResponseGood)
	} else {
		msg.SetSubject("EBC test: " + cfg.TestResponseBad)
	}

	msg.SetBody(smtp.TextHTML, sb.String())

	msg.Send(conn)
	fmt.Printf("%v sending test response to %v\n", logts(), from)
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
func storeTimeDB(t time.Time) string {

	res := t.Local().Format(timefmt)
	return res
}

func validateBonus(f4 fourFields) string {

	// We actually don't care about the points so drop them

	res, _ := fetchBonus(f4.BonusID, "bonuses")
	return res

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

func validateEntrant(f4 fourFields, from string) (bool, bool) {

	var allE []string
	if cfg.TestMode && !cfg.MatchEmail {
		allE = listValidTestAddresses()
	}

	sqlx := "SELECT RiderName,Email,TeamID FROM entrants WHERE EntrantID=" + strconv.Itoa(f4.EntrantID)
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

func imageFilename(imgid int, entrant int, bonus string, ext string) string {

	return "img" + "-" + strconv.Itoa(entrant) + "-" + bonus + "-" + strconv.Itoa(imgid) + ext

}
func writeImage(entrant int, bonus string, emailid uint32, pic []byte, filename string) int {

	var photoid int = 0

	if cfg.TestMode {
		return 0
	}

	fname := strings.ReplaceAll(filename, `"`, ``)

	ext := filepath.Ext(fname)

	isStd, _ := regexp.MatchString("\\"+standardimageextension, fname)
	isHeic := !isStd

	_, err := dbh.Exec("BEGIN TRANSACTION")
	if err != nil {
		if *verbose {
			fmt.Printf("%v can't store photo %v\n", logts(), err)
		}
		dbh.Exec("ROLLBACK")
		return 0
	}

	sqlx := "INSERT INTO ebcphotos(EntrantID,BonusID,EmailID) VALUES(?,?,?)"
	dbh.Exec(sqlx, entrant, bonus, emailid)
	row := dbh.QueryRow("SELECT last_insert_rowid()")
	row.Scan(&photoid)

	x := filepath.Join(cfg.Path2SM, cfg.ImageFolder, imageFilename(photoid, entrant, bonus, ext))
	err = os.WriteFile(x, pic, 0644)
	if err != nil {
		fmt.Printf("%v can't write image %v - error:%v\n", logts(), x, err)
		dbh.Exec("ROLLBACK")
		return 0
	}
	y := filepath.Join(cfg.ImageFolder, imageFilename(photoid, entrant, bonus, ext))

	if cfg.ConvertHeic && isHeic {
		y = filepath.Join(cfg.Path2SM, cfg.ImageFolder, imageFilename(photoid, entrant, bonus, standardimageextension))
		if *verbose {
			fmt.Printf(" Converting: %v %v %v\n", cfg.Heic2jpg, x, y)
		}
		cmd := exec.Command(cfg.Heic2jpg, x, y)
		err := cmd.Run()
		if err != nil {
			fmt.Printf("%v HEIC x %v FAILED %v\n", logts(), cfg.Heic2jpg, err)
			dbh.Exec("ROLLBACK")
			return 0
		}
		y = filepath.Join(cfg.ImageFolder, imageFilename(photoid, entrant, bonus, standardimageextension))

	}
	sqlx = "UPDATE ebcphotos SET image=? WHERE rowid=?"
	dbh.Exec(sqlx, y, photoid)
	dbh.Exec("COMMIT TRANSACTION")
	return photoid

}

func validateHeicHandler() {

	cmd := exec.Command(cfg.Heic2jpg)
	err := cmd.Run()
	if err != nil {
		fmt.Printf("%s: HEIC handler %v may not be available (%v)\n", apptitle, cfg.Heic2jpg, err)
		cfg.ConvertHeic = true // On Linux, "convert" returns 1 if run with no params
	}

}

func waitforkey() {

	fmt.Printf("%v: Press [Enter] to exit ... \n", apptitle)
	fmt.Scanln()

}

func yesno(x bool) string {
	if x {
		return ` <span style="` + ResponseStyleYes + `">&#x2714;</span>`
	}
	return ` <span style="` + ResponseStyleNo + `">&#x2718;</span>`
}
