package main

import (
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
}

type Fourfields struct {
	ok         bool
	EntrantID  int
	BonusID    string
	OdoReading int
	TimeHH     int
	TimeMM     int
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

func fetchNewClaims() {

	// Connect to server
	c, err := client.DialTLS(CFG.ImapServer, nil)
	if err != nil {
		log.Fatal(err)
	}

	// Don't forget to logout
	defer c.Logout()

	// Login
	if err := c.Login("ibaukebc@gmail.com", "9s&Nx#PTT"); err != nil {
		log.Fatal(err)
	}

	// Select INBOX
	_, err = c.Select("INBOX", false)
	if err != nil {
		log.Fatal(err)
	}
	criteria := imap.NewSearchCriteria()
	criteria.WithoutFlags = []string{"\\Flagged", "\\Seen"}
	criteria.SentSince = time.Date(2021, 7, 21, 0, 0, 0, 0, time.Local)
	uids, err := c.Search(criteria)
	if err != nil {
		log.Println(err)
	}

	seqset := new(imap.SeqSet)
	seqset.AddNum(uids...)
	if seqset.Empty() {
		return
	}
	log.Println(uids)
	messages := make(chan *imap.Message, 10)
	done := make(chan error, 1)
	go func() {
		done <- c.Fetch(seqset, []imap.FetchItem{imap.FetchEnvelope, imap.FetchFlags}, messages)
	}()

	if err := <-done; err != nil {
		return
	}

	log.Println("Matching messages:")
	seqSet := new(imap.SeqSet)
	for msg := range messages {
		fmt.Printf("%v = %v [%v]\n", msg.Envelope.Subject, msg.SeqNum, msg.Uid)
		seqSet.AddNum(msg.SeqNum)
	}
	item := imap.FormatFlagsOp(imap.AddFlags, true)
	flags := []interface{}{imap.FlaggedFlag}
	log.Printf("%v %v %v\n", seqSet, item, flags)
	err = c.Store(seqSet, item, flags, nil)
	if err != nil {
		log.Fatal(err)
	}

}

func init() {

	configPath := "ebcfetch.yml"

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
	//fmt.Printf("cfg == %v\n", CFG)
	for _, x := range []string{"1,2,3,12:11", "1 2 3 1211", "37, BB, 12345, 2317", "1 ca"} {
		f4 := parseSubject(x, false)
		if !f4.ok {
			fmt.Printf("%v == FAILED\n", x)
		} else {
			fmt.Printf("%v == Rider:%v, Bonus:%v, Odo:%v, HH:%v MM:%v\n", x, f4.EntrantID, f4.BonusID, f4.OdoReading, f4.TimeHH, f4.TimeMM)
		}
		//fmt.Printf("%v == %v == %v\n", x, CFG.SubjectRE.FindStringSubmatch(x), CFG.StrictRE.FindStringSubmatch(x))
	}

}

func main() {
	fetchNewClaims()
}
