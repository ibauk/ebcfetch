package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	_ "embed"

	yaml "gopkg.in/yaml.v2"
)

//go:embed ebcfetch.yml
var basicCfg string

var ReloadConfigFromDB bool

type EmailSettings struct {
	Port     int    `json:"Port"`
	Host     string `json:"Host"`
	Username string `json:"Username"`
	Password string `json:"Password"`
	CertName string `json:"CertName"`
}

// This MUST match Chasm's own declaration
type chasmEmailSettings struct {
	DontRun  bool
	TestMode bool
	SMTP     struct {
		Host     string
		Port     string
		Userid   string
		Password string
		CertName string // May need to override the certificate name used for TLS
	}
	IMAP struct {
		Host      string
		Port      string
		Userid    string
		Password  string
		NotBefore string
		NotAfter  string
	}
}
type chasmRallyBasics struct {
	RallyTitle        string
	RallyStarttime    string
	RallyFinishtime   string
	RallyMaxHours     int
	RallyUnitKms      bool
	RallyTimezone     string
	RallyPointIsComma bool
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
	TestModeLiteral       string             `yaml:"TestModeLiteral"`
	TestResponseSubject   string             `yaml:"TestResponseSubject"`
	TestResponseGood      string             `yaml:"TestResponseGood"`
	TestResponseBad       string             `yaml:"TestResponseBad"`
	TestResponseAdvice    string             `yaml:"TestResponseAdvice"`
	TestResponseBCC       string             `yaml:"TestResponseBCC"`
	TestResponseBadEmail  string             `yaml:"TestResponseBadEmail"`
	TestResponseGoodEmail string             `yaml:"TestResponseGoodEmail"`
	MaxExtraPhotos        int                `yaml:"MaxExtraPhotos"`
	DebugVerbose          bool               `yaml:"verbose"`
	MaxFetch              int                `yaml:"MaxFetch"`
	Email                 chasmEmailSettings `json:"Email"`
	Basics                chasmRallyBasics   `json:"Basics"`
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

func init() {

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

	file := strings.NewReader(basicCfg)

	D := yaml.NewDecoder(file)
	D.Decode(&cfg)

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
		os.Exit(1)
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

func fetchConfigFromChasmDB() []byte {
	rows, err := dbh.Query("SELECT Settings FROM config")
	if err != nil {
		fmt.Printf("%s can't fetch config from chasm database [%v] run aborted\n", logts(), err)
		osExit(1)
	}
	defer rows.Close()
	rows.Next()
	var json []byte
	rows.Scan(&json)
	return json

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

func loadRallyData() bool {

	var err error
	var RallyStart, RallyFinish, LocalTZ string

	if !*usingchasm {
		rows, err := dbh.Query("SELECT RallyTitle, StartTime as RallyStart,FinishTime as RallyFinish,LocalTZ FROM rallyparams")
		if err != nil {
			fmt.Printf("%s: OMG %v\n", apptitle, err)
			osExit(1)
		}
		defer rows.Close()
		rows.Next()
		rows.Scan(&cfg.RallyTitle, &RallyStart, &RallyFinish, &LocalTZ)

		cfg.LocalTimezone = LocalTZ
	}
	cfg.LocalTZ, err = time.LoadLocation(cfg.LocalTimezone)
	if err != nil {
		fmt.Printf("%s Timezone %s cannot be loaded\n", apptitle, cfg.LocalTimezone)
		return false
	}

	// Make the rally timezone our timezone, regardless of what the server is set to
	time.Local = cfg.LocalTZ

	if !*usingchasm {
		cfg.RallyStart, err = time.ParseInLocation("2006-01-02T15:04", RallyStart, cfg.LocalTZ)
		if err != nil {
			fmt.Printf("%s RallyStart %s cannot be parsed\n", apptitle, RallyStart)
			return false
		}
	} else {
		cfg.RallyStart, err = time.ParseInLocation("2006-01-02T15:04", cfg.Basics.RallyStarttime, cfg.LocalTZ)
		if err != nil {
			fmt.Printf("%s RallyStart %s cannot be parsed\n", apptitle, cfg.Basics.RallyStarttime)
			return false
		}
	}

	cfg.OffsetTZ = calcOffsetString(cfg.RallyStart)
	if *verbose {
		fmt.Printf("%s: Rally timezone is %v [%v]\n", apptitle, cfg.LocalTZ, cfg.OffsetTZ)
	}
	if !*usingchasm {
		cfg.RallyFinish, err = time.ParseInLocation("2006-01-02T15:04", RallyFinish, cfg.LocalTZ)
		if err != nil {
			fmt.Printf("%s RallyFinish %s cannot be parsed\n", apptitle, RallyFinish)
			return false
		}
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

func okFalseString(b bool) string {

	if b {
		return "ok"
	}
	return "FALSE"
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

func refreshConfig() {

	var ymltext string
	var jsontext []byte
	var err error

	if *usingchasm {
		jsontext = fetchConfigFromChasmDB()
		json.Unmarshal(jsontext, &cfg)
		cfg.ImapServer = cfg.Email.IMAP.Host + ":" + cfg.Email.IMAP.Port
		cfg.ImapLogin = cfg.Email.IMAP.Userid
		cfg.ImapPassword = cfg.Email.IMAP.Password
		cfg.LocalTimezone = cfg.Basics.RallyTimezone
		cfg.LocalTZ, err = time.LoadLocation(cfg.LocalTimezone)
		if err != nil {
			fmt.Printf("%s Timezone %s cannot be loaded\n", apptitle, cfg.LocalTimezone)
		}

		cfg.NotBefore = parseTime(cfg.Email.IMAP.NotBefore)
		cfg.NotAfter = parseTime(cfg.Email.IMAP.NotAfter)
		cfg.RallyTitle = cfg.Basics.RallyTitle
		cfg.RallyStart = parseTime(cfg.Basics.RallyStarttime)
		cfg.RallyFinish = parseTime(cfg.Basics.RallyFinishtime)
		cfg.SmtpStuff.Host = cfg.Email.SMTP.Host
		cfg.SmtpStuff.Port, _ = strconv.Atoi(cfg.Email.SMTP.Port)
		cfg.SmtpStuff.Username = cfg.Email.SMTP.Userid
		cfg.SmtpStuff.Password = cfg.Email.SMTP.Password
		cfg.SmtpStuff.CertName = cfg.Email.SMTP.CertName
		cfg.DontRun = cfg.Email.DontRun
		cfg.TestMode = cfg.Email.TestMode

		if false {
			b, err := json.MarshalIndent(cfg, "", "\t")
			if err != nil {
				fmt.Print("omg - json")
			}
			fmt.Printf("%v\n", string(b))
		}
	} else {

		ymltext, jsontext = fetchConfigFromDB()
		file := strings.NewReader(ymltext)
		D := yaml.NewDecoder(file)
		D.Decode(&cfg)
		json.Unmarshal(jsontext, &cfg.SmtpStuff)
	}
	if cfg.DebugVerbose {
		*verbose = true
	}
	cfg.Path2SM = filepath.Dir(*path2db)

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
