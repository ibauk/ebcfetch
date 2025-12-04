package main

import (
	"crypto/tls"
	"fmt"
	"strconv"
	"strings"
	"time"

	smtp "github.com/xhit/go-simple-mail/v2"
)

const ResponseStyleYes = ` font-size: large; color: lightgreen; `
const ResponseStyleNo = ` font-size: x-large; color: red; `
const ResponseStyleLbl = ` text-align: right; padding-right: 1em; `

func sendAlertToBob(whatsup string) {

	var sendToAddress = []string{"stammers.bob@gmail.com", "webmaster@ironbutt.co.uk"}
	const alertSubject = "EBCFetch alert"

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

func yesno(x bool) string {
	if x {
		return ` <span style="` + ResponseStyleYes + `">&#x2714;</span>`
	}
	return ` <span style="` + ResponseStyleNo + `">&#x2718;</span>`
}
