package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	_ "github.com/mattn/go-sqlite3"
	yaml "gopkg.in/yaml.v2"
)

var verbose = flag.Bool("v", false, "Verbose")
var silent = flag.Bool("s", false, "Silent")
var yml = flag.String("cfg", "ebcfetch.yml", "Path of YAML config file")

const apptitle = "EBCFetch v1.0"
const timefmt = time.RFC3339

var DBH *sql.DB

var CFG struct {
	ImapServer   string    `yaml:"imapserver"`
	ImapLogin    string    `yaml:"login"`
	ImapPassword string    `yaml:"password"`
	NotBefore    time.Time `yaml:"notbefore,omitempty"`
	NotAfter     time.Time `yaml:"notafter,omitempty"`
	Path2DB      string    `yaml:"db"`
	Subject      string    `yaml:"subject"`
	Strict       string    `yaml:"strict"`
	SubjectRE    *regexp.Regexp
	StrictRE     *regexp.Regexp
	RallyStart   time.Time
	RallyFinish  time.Time
	LocalTZ      *time.Location
}

type Fourfields struct {
	ok         bool
	EntrantID  int
	BonusID    string
	OdoReading int
	TimeHH     int
	TimeMM     int
}

func calcClaimDate(hh, mm int, rfc822date time.Time) time.Time {
	/*
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
	if CFG.RallyStart == CFG.RallyFinish {
		year, mth, day = CFG.RallyStart.Date()
	} else {
		year, mth, day = rfc822date.Date()
	}
	cd := time.Date(year, mth, day, hh, mm, 0, 0, CFG.LocalTZ)
	hrs := cd.Sub(rfc822date).Hours()
	fmt.Printf("Hrs = %v\n", hrs)
	if hrs > 1 && cd.Day() != CFG.RallyStart.Day() { // Claimed time is more than one hour later than the send (Date:) time of the email
		cd = cd.AddDate(0, 0, -1)
	}
	return cd
}

func storeTimeDB(t time.Time) string {

	res := t.Local().Format(timefmt)
	return res
}
func parseSubject(s string, formal bool) *Fourfields {

	var f4 Fourfields
	var ff []string

	if formal {
		ff = CFG.StrictRE.FindStringSubmatch(s)
	} else {
		ff = CFG.SubjectRE.FindStringSubmatch(s)
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
	hm, _ := strconv.Atoi(hmx)
	f4.TimeHH = hm / 100
	f4.TimeMM = hm % 100

	return &f4
}

func validateEntrant(f4 Fourfields) bool {

	for _, n := range []int{1, 2, 3} {
		if n == f4.EntrantID {
			return true
		}
	}
	return false
}

func validateBonus(f4 Fourfields) bool {
	for _, x := range []string{"A", "BB", "BA"} {
		if x == f4.BonusID {
			return true
		}
	}
	return false
}
func fetchNewClaims() {

	// Connect to server
	c, err := client.DialTLS(CFG.ImapServer, nil)
	if err != nil {
		log.Fatal(err)
	}

	// Don't forget to logout
	defer c.Logout()

	// Login
	if err := c.Login(CFG.ImapLogin, CFG.ImapPassword); err != nil {
		log.Fatal(err)
	}

	// Select INBOX
	_, err = c.Select("INBOX", false)
	if err != nil {
		log.Fatal(err)
	}
	criteria := imap.NewSearchCriteria()
	criteria.WithoutFlags = []string{"\\Flagged", "\\Seen"}
	criteria.SentSince = CFG.NotBefore
	uids, err := c.Search(criteria)
	if err != nil {
		log.Println(err)
	}

	seqset := new(imap.SeqSet)
	seqset.AddNum(uids...)
	if seqset.Empty() {
		return
	}
	if *verbose {
		fmt.Printf("Fetching %v\n", uids)
	}
	messages := make(chan *imap.Message, 10)
	done := make(chan error, 1)
	go func() {
		done <- c.Fetch(seqset, []imap.FetchItem{imap.FetchEnvelope, imap.FetchFlags, imap.FetchUid, imap.FetchInternalDate}, messages)
	}()

	if err := <-done; err != nil {
		return
	}

	if *verbose {
		log.Println("Matching messages:")
	}
	seqSet := new(imap.SeqSet)
	for msg := range messages {
		f4 := parseSubject(msg.Envelope.Subject, false)
		if f4.ok {
			f4.ok = validateEntrant(*f4) && validateBonus(*f4)
		}
		var sb strings.Builder
		sb.WriteString("INSERT INTO ebclaims (LoggedAt,DateTime,EntrantID,BonusID,OdoReading,FinalTime,EmailID,ClaimHH,ClaimMM,ClaimTime) VALUES(?,?,?,?,?,?,?,?,?,?)")
		_, err := DBH.Exec(sb.String(), storeTimeDB(time.Now()), storeTimeDB(msg.Envelope.Date.Local()),
			f4.EntrantID, f4.BonusID, f4.OdoReading,
			storeTimeDB(msg.InternalDate), msg.Uid, f4.TimeHH, f4.TimeMM,
			storeTimeDB(calcClaimDate(f4.TimeHH, f4.TimeMM, msg.Envelope.Date)))
		if err != nil {
			fmt.Printf("Can't store claim - %v\n", err)
		}
		if *verbose {
			fmt.Printf("%v = %v [%v] = %v\n", msg.Envelope.Subject, msg.SeqNum, msg.Uid, f4.ok)
		}
		seqSet.AddNum(msg.Uid)
	}
	item := imap.FormatFlagsOp(imap.AddFlags, true)
	flags := []interface{}{imap.FlaggedFlag}
	if *verbose {
		fmt.Printf("%v %v %v\n", seqSet, item, flags)
	}
	err = c.UidStore(seqSet, item, flags, nil)
	if err != nil {
		log.Fatal(err)
	}

}

func init() {

	flag.Parse()
	configPath := *yml

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return
	}

	file, err := os.Open(configPath)
	if err != nil {
		return
	}
	defer file.Close()

	D := yaml.NewDecoder(file)
	D.Decode(&CFG)
	CFG.StrictRE = regexp.MustCompile(CFG.Strict)
	CFG.SubjectRE = regexp.MustCompile(CFG.Subject)

	DBH, err = sql.Open("sqlite3", CFG.Path2DB)
	if err != nil {
		panic(err)
	}
	loadRallyData()
}

func loadRallyData() {

	rows, err := DBH.Query("SELECT StartTime as RallyStart,FinishTime as RallyFinish,LocalTZ FROM rallyparams")
	if err != nil {
		fmt.Printf("OMG %v\n", err)
	}
	defer rows.Close()
	rows.Next()
	var RallyStart, RallyFinish, LocalTZ string
	rows.Scan(&RallyStart, &RallyFinish, &LocalTZ)
	CFG.LocalTZ, _ = time.LoadLocation(LocalTZ)
	CFG.RallyStart, _ = time.ParseInLocation("2006-01-02T15:04", RallyStart, CFG.LocalTZ)
	CFG.RallyFinish, _ = time.ParseInLocation("2006-01-02T15:04", RallyFinish, CFG.LocalTZ)

	if *verbose {
		fmt.Printf("Rally dates = %v - %v\n", storeTimeDB(CFG.RallyStart), storeTimeDB(CFG.RallyFinish))
	}

}

func main() {
	if !*silent {
		fmt.Printf("%v\nCopyright (c) 2021 Bob Stammers\n", apptitle)
	}
	fetchNewClaims()
}
