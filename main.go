// EBCFetch:
// I fetch bonus claims from email and store in ScoreMaster database
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	_ "github.com/mattn/go-sqlite3"
	yaml "gopkg.in/yaml.v2"
)

const progdesc = `
I extract Electronic Bonus Claims from the designated email account using IMAP and load them into 
the scoring database ready for judging by a human being.

I parse the Subject line for the four fields entrant, bonus, odo and time and load the single photo
into the database. If the Subject line doesn't parse correctly, or if either the entrant or bonus
codes are not present in the database, or there's more than one photo, I "unsee" the email and 
don't record it in the database. Such unseen emails must be processed by hand. Look for unread 
emails in a Gmail window.  Photos are assumed to be JPGs.

If using Gmail, "Less secure apps access" must be enabled. To do that, edit the Google Account
settings, [Security]. Then enable the setting. Check that this is still enabled shortly before the
rally as Google resets it automatically after a while.`

var verbose = flag.Bool("v", false, "Verbose")
var silent = flag.Bool("s", false, "Silent")
var yml = flag.String("cfg", "ebcfetch.yml", "Path of YAML config file")
var showusage = flag.Bool("?", false, "Show this help text")

const apptitle = "EBCFetch v1.0"
const timefmt = time.RFC3339

var dbh *sql.DB

var cfg struct {
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
	CheckStrict  bool     `yaml:"checkstrict"`
	SleepSeconds int      `yaml:"sleepseconds"`
	ImageFolder  string   `yaml:"imagefolder"`
}

// fourFields: this contains the results of parsing the Subject line.
// The "four fields" are entrant, bonus, odo & claimtime
type fourFields struct {
	ok         bool
	EntrantID  int
	BonusID    string
	OdoReading int
	TimeHH     int
	TimeMM     int
	Extra      string
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
	return string(sm[1])

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

func fetchBonus(b string, t string) (bool, int) {

	rows, err := dbh.Query("SELECT BriefDesc,Points FROM "+t+" WHERE BonusID=?", b)
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
	c, err := client.DialTLS(cfg.ImapServer, nil)
	if err != nil {
		log.Println(err)
		return
	}

	// Don't forget to logout
	defer c.Logout()

	// Login
	if err := c.Login(cfg.ImapLogin, cfg.ImapPassword); err != nil {
		log.Fatal(err)
	}

	// Select INBOX
	_, err = c.Select("INBOX", false)
	if err != nil {
		log.Fatal(err)
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
		fmt.Printf("Searching ... ")
	}
	uids, err := c.Search(criteria)
	if err != nil {
		log.Println(err)
	}
	if *verbose {
		fmt.Printf("ok\n")
	}

	seqset := new(imap.SeqSet)
	seqset.AddNum(uids...)
	if seqset.Empty() {
		return
	}
	if *verbose {
		fmt.Printf("Fetching %v message(s)\n", len(uids))
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

		r := msg.GetBody(section) // This automatically marks the message as 'read'
		if r == nil {
			log.Fatal("Server didn't returned message body")
		}
		m, err := Parse(r)
		if err != nil {
			log.Fatal(err)
		}

		f4 := parseSubject(m.Subject, false)

		if !f4.ok || !validateEntrant(*f4) || !validateBonus(*f4) {
			if !*silent {
				fmt.Printf("Skipping %v [%v]\n", m.Subject, msg.Uid)
			}
			dealtwith.AddNum(msg.Uid) // Can't / won't process but don't want to see it again
			continue
		}

		var strictok bool = true
		if cfg.CheckStrict {
			f5 := parseSubject(m.Subject, true)
			strictok = f5.ok
		}

		var photoid int = 0
		var photoTime time.Time
		var numphotos int = 0
		var photosok bool = true
		for _, a := range m.Attachments {
			//fmt.Printf("Att: CD = %v\n", a.ContentDisposition)
			pt := timeFromPhoto(a.Filename, a.ContentDisposition)
			numphotos++
			if pt.After(photoTime) {
				photoTime = pt
			}
			pix, err := ioutil.ReadAll(a.Data)
			if err != nil {
				if !*silent {
					fmt.Printf("Attachment error %v\n", err)
					photosok = false
					break
				}
			} else {
				photoid = writeImage(f4.EntrantID, f4.BonusID, msg.Uid, pix)
				if photoid == 0 {
					photosok = false
					break
				}
				if *verbose {
					fmt.Printf("Attachment of size %v bytes\n", len(pix))
				}
			}
			//fmt.Printf("  Photo: %v\n", pt.Format(myTimeFormat))
		}
		for _, a := range m.EmbeddedFiles {
			//fmt.Printf("Emm: CD = %v\n", a.ContentDisposition)
			pt := timeFromPhoto(nameFromContentType(a.ContentType), a.ContentDisposition)
			numphotos++
			if pt.After(photoTime) {
				photoTime = pt
			}
			pix, err := ioutil.ReadAll(a.Data)
			if err != nil {
				if !*silent {
					fmt.Printf("Embedding error %v\n", err)
					photosok = false
					break
				}
			} else {
				photoid = writeImage(f4.EntrantID, f4.BonusID, msg.Uid, pix)
				if photoid == 0 {
					photosok = false
					break
				}
				if *verbose {
					fmt.Printf("Embedded image of size %v bytes\n", len(pix))
				}
			}
			if *verbose {
				fmt.Printf("  Photo: %v\n", pt.Format(myTimeFormat))
			}
		}

		if !photosok {
			skipped.AddNum(msg.Uid)
			continue
		}
		if numphotos > 1 {
			if !*silent {
				fmt.Printf("Skipping %v [%v] multiple photos\n", m.Subject, msg.Uid)
			}
			dealtwith.AddNum(msg.Uid)
			continue
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
			fmt.Printf("Can't store claim - %v\n", err)
			skipped.AddNum(msg.Uid) // Can't process now but I'll try again later
			continue

		}
		if !*silent {
			fmt.Printf("Claiming %v\n", m.Subject)
		}
		autoclaimed.AddNum(msg.Uid)

		if *verbose {
			fmt.Printf("%v  [%v] = %v\n", m.Subject, msg.Uid, strictok)
		}
	}

	if err := <-done; err != nil {
		fmt.Printf("OMG!!\n")
		log.Fatal(err)
	}

	if !autoclaimed.Empty() {
		item := imap.FormatFlagsOp(imap.AddFlags, true)
		flags := []interface{}{imap.FlaggedFlag, imap.SeenFlag}
		if *verbose {
			fmt.Printf("Claimed %v %v %v\n", autoclaimed, item, flags)
		}
	}
	if !dealtwith.Empty() {
		item := imap.FormatFlagsOp(imap.SetFlags, true)
		flags := []interface{}{imap.FlaggedFlag}
		if *verbose {
			fmt.Printf("Leaving unread %v %v %v\n", dealtwith, item, flags)
		}
		err = c.UidStore(dealtwith, item, flags, nil)
		if err != nil {
			log.Fatal(err)
		}
	}
	if !skipped.Empty() { // These are not yet dealt with
		item := imap.FormatFlagsOp(imap.SetFlags, true)
		flags := []interface{}{}
		if *verbose {
			fmt.Printf("Releasing %v %v %v\n", skipped, item, flags)
		}
		err = c.UidStore(skipped, item, flags, nil)
		if err != nil {
			log.Fatal(err)
		}
	}

}

func init() {

	flag.Usage = func() {
		w := flag.CommandLine.Output()
		fmt.Fprintf(w, "%v\n", apptitle)
		flag.PrintDefaults()
		fmt.Fprintf(w, "%v\n", progdesc)
	}
	flag.Parse()
	if *showusage {
		flag.Usage()
		os.Exit(1)
	}
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
	D.Decode(&cfg)
	cfg.StrictRE = regexp.MustCompile(cfg.Strict)
	cfg.SubjectRE = regexp.MustCompile(cfg.Subject)

	dbh, err = sql.Open("sqlite3", cfg.Path2DB)
	if err != nil {
		panic(err)
	}
	loadRallyData()
}

func loadRallyData() {

	rows, err := dbh.Query("SELECT RallyTitle, StartTime as RallyStart,FinishTime as RallyFinish,LocalTZ FROM rallyparams")
	if err != nil {
		fmt.Printf("OMG %v\n", err)
	}
	defer rows.Close()
	rows.Next()
	var RallyStart, RallyFinish, LocalTZ string
	rows.Scan(&cfg.RallyTitle, &RallyStart, &RallyFinish, &LocalTZ)
	cfg.LocalTZ, _ = time.LoadLocation(LocalTZ)
	cfg.RallyStart, _ = time.ParseInLocation("2006-01-02T15:04", RallyStart, cfg.LocalTZ)
	cfg.OffsetTZ = calcOffsetString(cfg.RallyStart)
	//fmt.Printf("%v\n", cfg.OffsetTZ)
	cfg.RallyFinish, _ = time.ParseInLocation("2006-01-02T15:04", RallyFinish, cfg.LocalTZ)

}

func main() {
	if !*silent {
		fmt.Printf("%v\nCopyright (c) 2021 Bob Stammers\n", apptitle)
		fmt.Printf("Monitoring %v for %v\n", cfg.ImapLogin, cfg.RallyTitle)
	}
	for {
		fetchNewClaims()
		time.Sleep(time.Duration(cfg.SleepSeconds) * time.Second)
	}
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

func validateBonus(f4 fourFields) bool {

	// We actually don't care about the points so drop them

	res, _ := fetchBonus(f4.BonusID, "bonuses")
	if res {
		return true
	}
	res, _ = fetchBonus(f4.BonusID, "specials")
	return res

}

func validateEntrant(f4 fourFields) bool {

	rows, err := dbh.Query("SELECT RiderName,Email FROM entrants WHERE EntrantID=?", f4.EntrantID)
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

func imageFilename(imgid int, entrant int, bonus string) string {

	return "img" + "-" + strconv.Itoa(entrant) + "-" + bonus + "-" + strconv.Itoa(imgid) + ".jpg"

}
func writeImage(entrant int, bonus string, emailid uint32, pic []byte) int {

	var photoid int = 0

	_, err := dbh.Exec("BEGIN TRANSACTION")
	if err != nil {
		if *verbose {
			fmt.Printf("Can't store photo %v\n", err)
		}
		return 0
	}

	sqlx := "INSERT INTO ebcphotos(EntrantID,BonusID,EmailID) VALUES(?,?,?)"
	dbh.Exec(sqlx, entrant, bonus, emailid)
	row := dbh.QueryRow("SELECT last_insert_rowid()")
	row.Scan(&photoid)

	x := filepath.Join(cfg.ImageFolder, imageFilename(photoid, entrant, bonus))
	err = ioutil.WriteFile(x, pic, 0644)
	if err != nil {
		fmt.Printf("Can't write image %v - error:%v\n", x, err)
	}
	sqlx = "UPDATE ebcphotos SET image=? WHERE rowid=?"
	dbh.Exec(sqlx, x, photoid)
	dbh.Exec("COMMIT TRANSACTION")
	return photoid

}
