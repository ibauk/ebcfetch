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

	"github.com/DusanKasan/parsemail"
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
	RallyTitle   string
	RallyStart   time.Time
	RallyFinish  time.Time
	LocalTZ      *time.Location
	OffsetTZ     string
	SelectFlags  []string `yaml:"selectflags"`
	BadBonus     int      `yaml:"bonusbad"`
	BadEntrant   int      `yaml:"entrantbad"`
	CheckStrict  bool     `yaml:"checkstrict"`
	SleepSeconds int      `yaml:"sleepseconds"`
}

// This contains the results of parsing the Subject line.
// The "four fields" are entrant, bonus, odo & claimtime
type Fourfields struct {
	ok         bool
	EntrantID  int
	BonusID    string
	OdoReading int
	TimeHH     int
	TimeMM     int
	Extra      string
}

const myTimeFormat = "2006-01-02 15:04:05"

type TIMESTAMP struct {
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

func timeFromPhoto(fname string) time.Time {

	tt, err := time.Parse(time.RFC3339, fname[0:4]+"-"+fname[4:6]+"-"+fname[6:8]+"T"+fname[9:11]+":"+fname[11:13]+":"+fname[13:15]+CFG.OffsetTZ)
	if err != nil {
		fmt.Printf("%v\n", err)
	}
	return tt
}

func nameFromContentType(ct string) string {
	re := regexp.MustCompile(`\"(.+)\"`)
	sm := re.FindSubmatch([]byte(ct))
	//fmt.Printf("%v %v\n", string(sm[0]), string(sm[1]))
	return string(sm[1])
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
	if hrs > 1 && cd.Day() != CFG.RallyStart.Day() { // Claimed time is more than one hour later than the send (Date:) time of the email
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
	res = res + strconv.Itoa(hrs) + "00"
	return res

}

func fetchBonus(b string, t string) (bool, int) {

	rows, err := DBH.Query("SELECT BriefDesc,Points FROM "+t+" WHERE BonusID=?", b)
	if err != nil {
		fmt.Printf("Bonus! %v %v\n", b, err)
		return false, 0
	}
	defer rows.Close()
	if !rows.Next() {
		return false, 0
	}

	var BriefDesc string
	var Points int
	rows.Scan(&BriefDesc, &Points)
	return BriefDesc != "", Points

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
	criteria.WithoutFlags = CFG.SelectFlags
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
		fmt.Printf("Fetching %v message(s)\n", len(uids))
	}

	// Get the whole message body
	section := &imap.BodySectionName{}
	items := []imap.FetchItem{section.FetchItem(), imap.FetchUid, imap.FetchInternalDate}

	messages := make(chan *imap.Message, 10)
	done := make(chan error, 1)
	go func() {
		done <- c.Fetch(seqset, items, messages)
	}()

	dealtwith := new(imap.SeqSet)
	autoclaimed := new(imap.SeqSet)
	skipped := new(imap.SeqSet)

	for msg := range messages {

		r := msg.GetBody(section)
		if r == nil {
			log.Fatal("Server didn't returned message body")
		}
		m, err := parsemail.Parse(r)
		if err != nil {
			log.Fatal(err)
		}

		dealtwith.AddNum(msg.Uid) // Add msg to queue to be flagged

		f4 := parseSubject(m.Subject, false)

		if !f4.ok || !validateEntrant(*f4) {
			if *verbose {
				fmt.Printf("Skipping %v [%v]\n", m.Subject, msg.Uid)
			}
			skipped.AddNum(msg.Uid)
			continue
		}

		autoclaimed.AddNum(msg.Uid)

		var decision int = -1

		ok, pts := validateBonus(*f4)
		if !ok {
			decision = CFG.BadBonus
		}

		var strictok bool = true
		if CFG.CheckStrict {
			f5 := parseSubject(m.Subject, true)
			strictok = f5.ok
		}

		var photoTime time.Time
		for _, a := range m.Attachments {
			pt := timeFromPhoto(a.Filename)
			if pt.After(photoTime) {
				photoTime = pt
			}
			fmt.Printf("  Photo: %v\n", pt.Format(myTimeFormat))
		}
		for _, a := range m.EmbeddedFiles {
			pt := timeFromPhoto(nameFromContentType(a.ContentType))
			if pt.After(photoTime) {
				photoTime = pt
			}
			fmt.Printf("  Photo: %v\n", pt.Format(myTimeFormat))
		}
		var sentatTime time.Time = msg.InternalDate
		for _, xr := range m.Header["X-Received"] {
			ts := TIMESTAMP{parseTime(extractTime(xr)).Local()}
			if ts.date.Before(sentatTime) {
				sentatTime = ts.date
			}
		}
		for _, xr := range m.Header["Received"] {
			ts := TIMESTAMP{parseTime(extractTime(xr)).Local()}
			if ts.date.Before(sentatTime) {
				sentatTime = ts.date
			}
		}
		//xmitTime := latestTime.Sub(m.Date)

		var sb strings.Builder
		sb.WriteString("INSERT INTO ebclaims (LoggedAt,DateTime,EntrantID,BonusID,OdoReading,")
		sb.WriteString("FinalTime,EmailID,ClaimHH,ClaimMM,ClaimTime,Subject,ExtraField,Decision,")
		sb.WriteString("StrictOk,Points,AttachmentTime,FirstTime) ")
		sb.WriteString("VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)")
		_, err = DBH.Exec(sb.String(), storeTimeDB(time.Now()), storeTimeDB(m.Date.Local()),
			f4.EntrantID, f4.BonusID, f4.OdoReading,
			storeTimeDB(msg.InternalDate), msg.Uid, f4.TimeHH, f4.TimeMM,
			storeTimeDB(calcClaimDate(f4.TimeHH, f4.TimeMM, m.Date)),
			m.Subject, f4.Extra,
			decision, strictok, pts, photoTime, sentatTime)
		if err != nil {
			fmt.Printf("Can't store claim - %v\n", err)
		}
		if *verbose {
			fmt.Printf("%v  [%v] = %v\n", m.Subject, msg.Uid, strictok)
		}
	}

	if err := <-done; err != nil {
		fmt.Printf("OMG!!\n")
		log.Fatal(err)
	}

	item := imap.FormatFlagsOp(imap.AddFlags, true)
	flags := []interface{}{imap.FlaggedFlag}
	if *verbose {
		fmt.Printf("Touched %v %v %v\n", dealtwith, item, flags)
	}
	err = c.UidStore(dealtwith, item, flags, nil)
	if err != nil {
		log.Fatal(err)
	}
	if !autoclaimed.Empty() {
		flags = []interface{}{imap.SeenFlag}
		if *verbose {
			fmt.Printf("Claimed %v %v %v\n", autoclaimed, item, flags)
		}
		//err = c.UidStore(autoclaimed, item, flags, nil)
		//if err != nil {
		//	log.Fatal(err)
		//}
	}
	if !skipped.Empty() {
		item = imap.FormatFlagsOp(imap.SetFlags, true)
		if *verbose {
			fmt.Printf("Unseeing %v %v %v\n", skipped, item, flags)
		}
		err = c.UidStore(skipped, item, flags, nil)
		if err != nil {
			log.Fatal(err)
		}
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

	rows, err := DBH.Query("SELECT RallyTitle, StartTime as RallyStart,FinishTime as RallyFinish,LocalTZ FROM rallyparams")
	if err != nil {
		fmt.Printf("OMG %v\n", err)
	}
	defer rows.Close()
	rows.Next()
	var RallyStart, RallyFinish, LocalTZ string
	rows.Scan(&CFG.RallyTitle, &RallyStart, &RallyFinish, &LocalTZ)
	CFG.LocalTZ, _ = time.LoadLocation(LocalTZ)
	CFG.RallyStart, _ = time.ParseInLocation("2006-01-02T15:04", RallyStart, CFG.LocalTZ)
	CFG.OffsetTZ = calcOffsetString(CFG.RallyStart)
	CFG.RallyFinish, _ = time.ParseInLocation("2006-01-02T15:04", RallyFinish, CFG.LocalTZ)

}

func main() {
	if !*silent {
		fmt.Printf("%v\nCopyright (c) 2021 Bob Stammers\n", apptitle)
		fmt.Printf("Monitoring %v for %v\n", CFG.ImapLogin, CFG.RallyTitle)
	}
	for {
		fetchNewClaims()
		time.Sleep(time.Duration(CFG.SleepSeconds) * time.Second)
	}
}

func parseSubject(s string, formal bool) *Fourfields {

	//fmt.Printf("Parsing %v\n", s)
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

	if len(ff) > 5 {
		f4.Extra = ff[5]
	}

	return &f4
}

func storeTimeDB(t time.Time) string {

	res := t.Local().Format(timefmt)
	return res
}

func validateBonus(f4 Fourfields) (bool, int) {

	res, pts := fetchBonus(f4.BonusID, "bonuses")
	if res {
		return true, pts
	}
	res, pts = fetchBonus(f4.BonusID, "specials")
	return res, pts

}

func validateEntrant(f4 Fourfields) bool {

	rows, err := DBH.Query("SELECT RiderName,Email FROM entrants WHERE EntrantID=?", f4.EntrantID)
	if err != nil {
		fmt.Printf("Entrant! %v %v\n", f4.EntrantID, err)
		return false
	}
	defer rows.Close()
	if !rows.Next() {
		return false
	}

	var RiderName, Email string
	rows.Scan(&RiderName, &Email)
	return RiderName != ""
}
