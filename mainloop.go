package main

import (
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

var currentUid uint32 // Used to keep track of email fetch recovery
var lastGoodUid uint32

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

	uids, err := c.Search(criteria)
	if err != nil {
		log.Printf("Search: %v\n", err)
		return nil, nil
	}

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
	messages := make(chan *imap.Message, Nmax+1) // Buffered channel wide enough for all messages +1 for luck
	done := make(chan error, 1)
	go func() {
		defer close(done)
		done <- c.Fetch(seqset, items, messages)
	}()

	for msg := range messages {

		currentUid = msg.Uid

		N++
		if *verbose {
			fmt.Printf("Considering msg #%v=%v\n", N, currentUid)
		}

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

		var TR testResponse

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
				fmt.Printf("%v skipping %v [%v] ok=%v,ve=%v,vb=%v\n", logts(), m.Subject, msg.Uid, okFalseString(f4.ok), okFalseString(ve), okFalseString(vb != ""))
			}
			dealtwith.AddNum(msg.Uid) // Can't / won't process but don't want to see it again
			if !cfg.TestMode {
				continue
			}
		} else {

			TR.ClaimIsGood = f4.ok && ve && (vea || !cfg.MatchEmail) && vb != "" && f4.TimeOk

		}

		photosok, numphotos, photoids, photoTime := processImages(m, f4.EntrantID, f4.BonusID, msg.Uid)

		if photosok {
			TR.PhotoPresent = numphotos
		} else if numphotos > 0 {
			TR.PhotoPresent = 0 - numphotos
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
		}

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
		lastGoodUid = msg.Uid

		if !*silent {
			fmt.Printf("Claiming #%v [ %v ]\n", msg.Uid, m.Subject)
		}

	} // End msg loop

	if err := <-done; err != nil {
		if !*silent {
			log.Printf("%s OMG!! msg=%v (%v / %v) %v\n", logts(), currentUid, N, Nmax, err)
		}

		// This loop adds back EmailIDs that were potentially called
		// but not processed. It is possible that there were gaps in
		// the range called so need to add a 'sensible' overrun so:-

		release := Nmax - N
		for i := uint32(0); i <= uint32(release); i++ {
			if currentUid+i > lastGoodUid {
				skipped.AddNum(currentUid + i)
				if *verbose {
					log.Printf("%s OMG=%v\n", logts(), currentUid+i)
				}
			}
		}
	}

	return dealtwith, skipped

}

func flagSkippedEmails(ss *imap.SeqSet, ignoreThem bool) bool {

	// Connect to server
	c, err := client.DialTLS(cfg.ImapServer, nil)
	if err != nil {
		log.Printf("fse:DialTLS: %v\n", err)
		return false
	}

	// Don't forget to logout
	defer c.Logout()

	// Login
	if err := c.Login(cfg.ImapLogin, cfg.ImapPassword); err != nil {
		log.Printf("fse:Login: %v\n", err)
		return false
	}

	// Select INBOX
	_, err = c.Select("INBOX", false)
	if err != nil {
		log.Printf("fse:Select: %v\n", err)
		return false
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
		log.Printf("UidStore it=%v, failed %v\n%v\n", ignoreThem, err, ss)
		return false
	}
	return true

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

	return &f4
}

func safelyFlagSkippedEmails(ss *imap.SeqSet, ignoreThem bool) {

	const maxTries = 10 // Used to avoid the possible infinite loop

	if cfg.TestMode {
		return
	}

	if ss == nil {
		return
	}
	if ss.Empty() {
		return
	}
	n := maxTries
	for {
		if flagSkippedEmails(ss, ignoreThem) {
			return
		}
		n--
		if n < 1 {
			break
		}
	}
	log.Printf("OMG can't skip %v, it=%v\n", ss, ignoreThem)

}
