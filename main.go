// EBCFetch:
// I fetch bonus claims from email and store in ScoreMaster database
package main

import (
	"database/sql"
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

const progdesc = `
I extract Electronic Bonus Claims from the designated email account using IMAP
and load them into the scoring database ready for judging by a human being.

I parse the Subject line for the four fields entrant, bonus, odo and time and 
load the single photo into the database. If the Subject line doesn't parse 
correctly, or if either the entrant or bonus codes are not present in the 
database, or there's more than one photo, I "unsee" the email and don't record 
it in the database. Such unseen emails must be processed by hand. Look for 
unread emails in a Gmail window.  Photos are expected to be JPGs but HEICs are 
also accepted and can be automatically converted to JPG using an external 
utility.

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
const appversion = "1.7"
const timefmt = time.RFC3339

const ResponseStyleYes = ` font-size: large; color: green; `
const ResponseStyleNo = ` font-size: x-large; color: red; `
const ResponseStyleLbl = ` text-align: right; padding-right: 1em; `

var dbh *sql.DB

var cfg struct {
	ImapServer       string    `yaml:"imapserver"`
	ImapLogin        string    `yaml:"login"`
	ImapPassword     string    `yaml:"password"`
	NotBefore        time.Time `yaml:"notbefore,omitempty"`
	NotAfter         time.Time `yaml:"notafter,omitempty"`
	Path2DB          string    `yaml:"db"`
	Subject          string    `yaml:"subject"`
	Strict           string    `yaml:"strict"`
	SubjectRE        *regexp.Regexp
	StrictRE         *regexp.Regexp
	RallyTitle       string
	RallyStart       time.Time
	RallyFinish      time.Time
	LocalTZ          *time.Location
	OffsetTZ         string
	SelectFlags      []string `yaml:"selectflags"`
	CheckStrict      bool     `yaml:"checkstrict"`
	SleepSeconds     int      `yaml:"sleepseconds"`
	Path2SM          string   `yaml:"path2sm"`
	ImageFolder      string   `yaml:"imagefolder"`
	MatchEmail       bool     `yaml:"matchemail"`
	Heic2jpg         string   `yaml:"heic2jpg"`
	ConvertHeic      bool     `yaml:"convertheic2jpg"`
	DontRun          bool     `yaml:"dontrun"`
	KeyWait          bool     `yaml:"debugwait"`
	AllowBody        bool     `yaml:"allowbody"`
	TrapMails        bool     `yaml:"trapmails"`
	TrapPath         string   `yaml:"trappath"`
	TestMode         bool     `yaml:"testmode"`
	SMTPServer       string   `yaml:"SMTPServer"`
	SMTPUser         string   `yaml:"SMTPUser"`
	SMTPPassword     string   `yaml:"SMTPPassword"`
	TestResponseGood string   `yaml:"TestResponseGood"`
	TestResponseBad  string   `yaml:"TestResponseBad"`
}

// fourFields: this contains the results of parsing the Subject line.
// The "four fields" are entrant, bonus, odo & claimtime
type fourFields struct {
	ok         bool
	EntrantID  int
	BonusID    string
	OdoReading int
	HHmm       string
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
	 * The time of day is treated as local time.
	 *
	 * If the rally spans a single day, that day is applied. If not then the date is
	 * derived from the email's Date field unless that field refers to an earlier time
	 * of day in which case we use the previous day.
	 *
	 */

	var year, day int
	var mth time.Month
	if cfg.RallyStart == cfg.RallyFinish {
		year, mth, day = cfg.RallyStart.Date()
	} else {
		year, mth, day = rfc822date.Date()
	}
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

func fetchConfigFromDB() string {
	rows, err := dbh.Query("SELECT ebcsettings FROM rallyparams")
	if err != nil {
		fmt.Printf("%s can't fetch config from database [%v] run aborted\n", logts(), err)
		osExit(1)
	}
	defer rows.Close()
	rows.Next()
	var res string
	rows.Scan(&res)
	return res

}
func fetchNewClaims() {

	// Connect to server
	c, err := client.DialTLS(cfg.ImapServer, nil)
	if err != nil {
		log.Println(err)
		return
	}

	// Don't forget to logout
	defer c.Logout()

	// Login
	if err := c.Login(cfg.ImapLogin, cfg.ImapPassword); err != nil {
		log.Println(err)
		return
	}

	// Select INBOX
	_, err = c.Select("INBOX", false)
	if err != nil {
		log.Println(err)
		return
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

	if *verbose {
		fmt.Printf("%s searching ... ", logts())
	}
	uids, err := c.Search(criteria)
	if err != nil {
		log.Println(err)
	}
	if *verbose {
		fmt.Printf("%s ok\n", logts())
	}

	seqset := new(imap.SeqSet)
	seqset.AddNum(uids...)
	if seqset.Empty() {
		return
	}
	if *verbose {
		fmt.Printf("%s fetching %v message(s)\n", logts(), len(uids))
	}

	// Get the whole message body, automatically sets //Seen
	section := &imap.BodySectionName{}
	items := []imap.FetchItem{section.FetchItem(), imap.FetchUid, imap.FetchInternalDate}

	messages := make(chan *imap.Message, 10)
	done := make(chan error, 1)
	go func() {
		done <- c.Fetch(seqset, items, messages)
	}()

	autoclaimed := new(imap.SeqSet)
	skipped := new(imap.SeqSet)
	dealtwith := new(imap.SeqSet)

	for msg := range messages {

		var TR testResponse

		r := msg.GetBody(section) // This automatically marks the message as 'read'
		if r == nil {
			log.Println("Server didn't return message body")
			continue
		}
		m, err := Parse(r)
		if err != nil {
			log.Println(err)
			continue
		}

		if cfg.TrapMails && cfg.TrapPath != "" {
			fmt.Printf("INCOMING: %v\n", m)
		}

		f4 := parseSubject(m.Subject, false)
		if !f4.ok && cfg.AllowBody {
			f4 = parseSubject(m.TextBody, false)
			if f4.ok {
				m.Subject = m.TextBody
				TR.SubjectFromBody = true
			}
		}

		TR.ClaimSubject = m.Subject
		TR.EntrantID = f4.EntrantID
		TR.BonusID = f4.BonusID
		TR.OdoReading = f4.OdoReading
		TR.ClaimDateTime = calcClaimDate(f4.TimeHH, f4.TimeMM, m.Date)
		TR.HHmm = f4.HHmm
		TR.ExtraField = f4.Extra

		ve := validateEntrant(*f4, m.Header.Get("From"))
		TR.AddressIsRegistered = ve

		vb := validateBonus(*f4)
		TR.BonusIsReal = vb != ""
		TR.BonusDesc = vb

		if !f4.ok || !ve || vb == "" {
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
				//fmt.Printf("F4: E{%v} B{%v} O{%v} T{%v:%v}\n", f4.EntrantID, f4.BonusID, f4.OdoReading, f4.TimeHH, f4.TimeMM)
				fmt.Printf("%v skipping %v [%v] ok=%v,ve=%v,vb=%v\n", logts(), m.Subject, msg.Uid, okx, vex, vbx)
			}
			dealtwith.AddNum(msg.Uid) // Can't / won't process but don't want to see it again
			if !cfg.TestMode {
				continue
			}
		} else {

			TR.ClaimIsGood = true
		}

		var strictok bool = true
		if cfg.CheckStrict || cfg.TestMode {
			f5 := parseSubject(m.Subject, true)
			strictok = f5.ok
		}
		TR.ClaimIsPerfect = TR.ClaimIsGood && strictok

		var photoid int = 0
		var photoTime time.Time
		var numphotos int = 0
		var photosok bool = true
		var writefailure bool = false
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
				if photoid == 0 {
					photosok = false
					writefailure = true
					break
				}
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
				if photoid == 0 {
					photosok = false
					break
				}
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

		if !photosok {
			skipped.AddNum(msg.Uid)
			if !cfg.TestMode || writefailure {
				continue
			}
		}
		if photosok && numphotos < 1 {
			if !*silent {
				fmt.Printf("%s skipping %v [%v] no photo\n", logts(), m.Subject, msg.Uid)
			}
			dealtwith.AddNum(msg.Uid)
			if !cfg.TestMode || writefailure {
				continue
			}
		}
		if photosok && numphotos > 1 {
			if !*silent {
				fmt.Printf("%s skipping %v [%v] multiple photos\n", logts(), m.Subject, msg.Uid)
			}
			dealtwith.AddNum(msg.Uid)
			if !cfg.TestMode || writefailure {
				continue
			}
		}

		if cfg.TestMode {
			sendTestResponse(TR, m.Header.Get("From"))
			if !TR.ClaimIsGood || !photosok || numphotos != 1 {
				continue
			}

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

		var sb strings.Builder
		sb.WriteString("INSERT INTO ebclaims (LoggedAt,DateTime,EntrantID,BonusID,OdoReading,")
		sb.WriteString("FinalTime,EmailID,ClaimHH,ClaimMM,ClaimTime,Subject,ExtraField,")
		sb.WriteString("StrictOk,AttachmentTime,FirstTime,PhotoID) ")
		sb.WriteString("VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)")
		_, err = dbh.Exec(sb.String(), storeTimeDB(time.Now()), storeTimeDB(m.Date.Local()),
			f4.EntrantID, f4.BonusID, f4.OdoReading,
			storeTimeDB(msg.InternalDate), msg.Uid, f4.TimeHH, f4.TimeMM,
			storeTimeDB(calcClaimDate(f4.TimeHH, f4.TimeMM, m.Date)),
			m.Subject, f4.Extra,
			strictok, photoTime, sentatTime, photoid)
		if err != nil {
			if !*silent {
				fmt.Printf("%s can't store claim - %v\n", logts(), err)
			}
			skipped.AddNum(msg.Uid) // Can't process now but I'll try again later
			continue

		}
		if !*silent {
			fmt.Printf("%s claiming %v\n", logts(), m.Subject)
		}
		autoclaimed.AddNum(msg.Uid)

		if *verbose {
			var sok string = "!strictF4"
			if strictok {
				sok = "strictF4 ok"
			}
			fmt.Printf("%s %v  [msg.Uid %v] = %v\n", logts(), m.Subject, msg.Uid, sok)
		}
	}

	if err := <-done; err != nil {
		if !*silent {
			fmt.Printf("%s OMG!! %v\n", logts(), err)
		}
		return
	}

	if !autoclaimed.Empty() {
		item := imap.FormatFlagsOp(imap.AddFlags, true)
		flags := []interface{}{imap.FlaggedFlag, imap.SeenFlag}
		if *verbose {
			fmt.Printf("%s claimed %v %v %v\n", logts(), autoclaimed, item, flags)
		}
	}
	if !dealtwith.Empty() {
		item := imap.FormatFlagsOp(imap.SetFlags, true)
		flags := []interface{}{imap.FlaggedFlag}
		if *verbose {
			fmt.Printf("%s leaving unread %v %v %v\n", logts(), dealtwith, item, flags)
		}
		err = c.UidStore(dealtwith, item, flags, nil)
		if err != nil {
			log.Println(err)
			return
		}
	}
	if !skipped.Empty() { // These are not yet dealt with
		item := imap.FormatFlagsOp(imap.SetFlags, true)
		flags := []interface{}{}
		if *verbose {
			fmt.Printf("%s releasing %v %v %v\n", logts(), skipped, item, flags)
		}
		err = c.UidStore(skipped, item, flags, nil)
		if err != nil {
			log.Println(err)
			return
		}
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
		fmt.Printf("%v: v%v   Copyright (c) 2022 Bob Stammers\n", apptitle, appversion)
	}

	if *path2db == "" {
		fmt.Printf("%s No database has been specified Run aborted\n", apptitle)
		osExit(1)
	}

	openDB(*path2db)

	configPath := *yml

	if strings.EqualFold(configPath, "") {
		configPath = "config"
		ymltext := fetchConfigFromDB()
		file := strings.NewReader(ymltext)
		D := yaml.NewDecoder(file)
		D.Decode(&cfg)
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
	}

	cfg.Path2DB = *path2db

	if cfg.DontRun {
		if !*silent {
			fmt.Printf("%s: DontRun option triggered, enough already\n", apptitle)
		}
		osExit(0)
	}

	if cfg.ImapServer == "" || cfg.ImapLogin == "" {
		fmt.Printf("%s: Email configuration has not been specified\n", apptitle)
		fmt.Printf("%s: Email fetching will not be possible. Please fix %v and retry\n", apptitle, configPath)
		osExit(1)
	}
	if cfg.ImapPassword == "" {
		fmt.Printf("%s: No password has been set for incoming IMAP account %v\n", apptitle, cfg.ImapServer)
		fmt.Printf("%s: Email fetching will not be possible. Please fix %v and retry\n", apptitle, configPath)
		osExit(1)
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

	cfg.LocalTZ, err = time.LoadLocation(LocalTZ)
	if err != nil {
		fmt.Printf("%s Timezone %s cannot be loaded\n", apptitle, LocalTZ)
		return false
	}
	if *verbose {
		fmt.Printf("cfg.LocalTZ is %v\n", cfg.LocalTZ)
	}
	cfg.RallyStart, err = time.ParseInLocation("2006-01-02T15:04", RallyStart, cfg.LocalTZ)
	if err != nil {
		fmt.Printf("%s RallyStart %s cannot be parsed\n", apptitle, RallyStart)
		return false
	}
	cfg.OffsetTZ = calcOffsetString(cfg.RallyStart)
	if *verbose {
		fmt.Printf("cfg.OffsetTZ is %v\n", cfg.OffsetTZ)
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

func main() {

	if !*silent {
		fmt.Printf("%v: Monitoring %v for %v\n", apptitle, cfg.ImapLogin, cfg.RallyTitle)
	}

	for {
		fetchNewClaims()
		time.Sleep(time.Duration(cfg.SleepSeconds) * time.Second)
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
func parseSubject(s string, formal bool) *fourFields {

	//fmt.Printf("Parsing %v\n", s)
	var f4 fourFields
	var ff []string

	if formal {
		ff = cfg.StrictRE.FindStringSubmatch(s)
	} else {
		ff = cfg.SubjectRE.FindStringSubmatch(s)
	}
	f4.ok = len(ff) > 0
	if formal && len(ff) < 5 {
		f4.ok = false
	}
	if !f4.ok {
		return &f4
	}
	f4.EntrantID, _ = strconv.Atoi(ff[1])
	f4.BonusID = strings.ToUpper(ff[2])
	if len(ff) < 5 {
		return &f4
	}
	f4.OdoReading, _ = strconv.Atoi(ff[3])
	hmx := strings.ReplaceAll(strings.ReplaceAll(ff[4], ":", ""), ".", "")
	f4.HHmm = hmx
	hm, _ := strconv.Atoi(hmx)
	f4.TimeHH = hm / 100
	f4.TimeMM = hm % 100

	if len(ff) > 5 {
		f4.Extra = ff[5]
	}

	return &f4
}

/*
sendTestResponse generates and sends a narrative email to the sender
of any emails received while cfg.TestMode is true.
*/
func yesno(x bool) string {
	if x {
		return ` <span style="` + ResponseStyleYes + `">&#x2714;</span>`
	}
	return ` <span style="` + ResponseStyleNo + `">&#x2718;</span>`
}

func sendTestResponse(tr testResponse, from string) {

	var sb strings.Builder

	if tr.ClaimIsGood && tr.PhotoPresent > 0 {
		sb.WriteString("<p>" + cfg.TestResponseGood + "</p>")
	} else {
		sb.WriteString("<p>" + cfg.TestResponseBad + "</p>")
	}
	sb.WriteString("<table>")
	sb.WriteString(`<tr><td style="` + ResponseStyleLbl + `">Subject</td><td>`)
	sb.WriteString(tr.ClaimSubject)
	if tr.SubjectFromBody {
		sb.WriteString(" &#x2611;")
	}
	sb.WriteString(`</td></tr><tr><td style="` + ResponseStyleLbl + `">Email = Entrant<td>`)
	sb.WriteString(yesno(tr.AddressIsRegistered))
	sb.WriteString(`</td></tr><tr><td style="` + ResponseStyleLbl + `">Photo</td><td>`)
	sb.WriteString(strconv.Itoa(tr.PhotoPresent))

	sb.WriteString(yesno(tr.PhotoPresent > 0))

	sb.WriteString(`</td></tr><tr><td style="` + ResponseStyleLbl + `">Bonus</td><td>`)
	sb.WriteString(tr.BonusID)
	if tr.BonusIsReal {
		sb.WriteString(" - ")
		sb.WriteString(tr.BonusDesc)
	} else {
		sb.WriteString(yesno(false))
	}
	sb.WriteString(`</td></tr><tr><td style="` + ResponseStyleLbl + `">Odo</td><td>`)
	sb.WriteString(strconv.Itoa(tr.OdoReading))
	sb.WriteString(`</td></tr><tr><td style="` + ResponseStyleLbl + `">hhmm(` + tr.HHmm + `)</td><td>`)
	sb.WriteString(tr.ClaimDateTime.Format(time.UnixDate))
	if tr.ExtraField != "" {
		sb.WriteString(`</td></tr><tr><td style="` + ResponseStyleLbl + `">&#x270D;</td><td>`)
		sb.WriteString(tr.ExtraField)
	}
	sb.WriteString("</td></tr></table>")

	client := smtp.NewSMTPClient()
	client.Host = cfg.SMTPServer
	client.Port = 587
	client.Username = cfg.SMTPUser
	client.Password = cfg.SMTPPassword
	client.Encryption = smtp.EncryptionTLS
	client.ConnectTimeout = 10 * time.Second
	client.SendTimeout = 10 * time.Second
	client.KeepAlive = false

	conn, err := client.Connect()
	if err != nil {
		fmt.Printf("Can't connect to %v because %v\n", client.Host, err)
		return
	}
	msg := smtp.NewMSG()
	msg.AddTo(from)
	msg.SetFrom(cfg.ImapLogin)
	if tr.ClaimIsGood && tr.PhotoPresent > 0 {
		msg.SetSubject("EBCFetch: " + cfg.TestResponseGood)
	} else {
		msg.SetSubject("EBCFetch: " + cfg.TestResponseBad)
	}

	msg.SetBody(smtp.TextHTML, sb.String())

	msg.Send(conn)
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

func validateEntrant(f4 fourFields, from string) bool {

	rows, err := dbh.Query("SELECT RiderName,Email FROM entrants WHERE EntrantID=?", f4.EntrantID)
	if err != nil {
		fmt.Printf("%v Entrant! %v %v\n", logts(), f4.EntrantID, err)
		return false
	}
	defer rows.Close()
	if !rows.Next() {
		if !*silent {
			fmt.Printf("%v No such entrant %v\n", logts(), f4.EntrantID)
		}
		return false
	}

	var RiderName, Email string
	rows.Scan(&RiderName, &Email)
	v, _ := mail.ParseAddress(from)
	e, _ := mail.ParseAddressList(Email)
	ok := !cfg.MatchEmail
	if !ok || cfg.TestMode {
		if cfg.TestMode {
			ok = false
		}
		for _, em := range e {
			if *verbose {
				fmt.Printf("%v comparing %v\n", logts(), em.Address)
			}
			ok = ok || strings.EqualFold(em.Address, v.Address)
			if !ok {
				f := func(c rune) bool {
					return c == '@'
				}
				a1 := strings.FieldsFunc(em.Address, f)
				a2 := strings.FieldsFunc(v.Address, f)
				ok = strings.EqualFold(a1[0], a2[0]) // Compare only the 'account' port of the address
				if ok && !*silent {
					fmt.Printf("%v matched email from %v for rider %v <%v> [%v]\n", logts(), v.Address, RiderName, Email, ok)
				}
			}
		}
		if !ok && !*silent {
			fmt.Printf("%v received from %v for rider %v <%v> [%v]\n", logts(), v.Address, RiderName, Email, ok)
		}
	}
	return ok && !strings.EqualFold(RiderName, "")
}

func imageFilename(imgid int, entrant int, bonus string, isHeic bool) string {

	var ext string = ".jpg"
	if isHeic {
		ext = ".HEIC"
	}
	return "img" + "-" + strconv.Itoa(entrant) + "-" + bonus + "-" + strconv.Itoa(imgid) + ext

}
func writeImage(entrant int, bonus string, emailid uint32, pic []byte, filename string) int {

	var photoid int = 0

	isHeic, _ := regexp.MatchString("(?i)\\.heic", filename)
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

	x := filepath.Join(cfg.Path2SM, cfg.ImageFolder, imageFilename(photoid, entrant, bonus, isHeic))
	err = os.WriteFile(x, pic, 0644)
	if err != nil {
		fmt.Printf("%v can't write image %v - error:%v\n", logts(), x, err)
		dbh.Exec("ROLLBACK")
		return 0
	}
	y := filepath.Join(cfg.ImageFolder, imageFilename(photoid, entrant, bonus, isHeic))
	if cfg.ConvertHeic && isHeic {
		y = filepath.Join(cfg.Path2SM, cfg.ImageFolder, imageFilename(photoid, entrant, bonus, false))
		cmd := exec.Command(cfg.Heic2jpg, x, y)
		err := cmd.Run()
		if err != nil {
			fmt.Printf("%v HEIC x %v FAILED %v\n", logts(), cfg.Heic2jpg, err)
			dbh.Exec("ROLLBACK")
			return 0
		}
		y = filepath.Join(cfg.ImageFolder, imageFilename(photoid, entrant, bonus, false))

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
		fmt.Printf("%s: HEIC handler %v NOT AVAILABLE (%v)\n", apptitle, cfg.Heic2jpg, err)
		cfg.ConvertHeic = false
	}

}

func waitforkey() {

	fmt.Printf("%v: Press [Enter] to exit ... \n", apptitle)
	fmt.Scanln()

}
