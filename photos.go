package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// I'll pass files without this extension to ebcimg for conversion
const standardimageextension = ".jpg"

func imageFilename(imgid int, entrant int, bonus string, ext string) string {

	return "img" + "-" + strconv.Itoa(entrant) + "-" + bonus + "-" + strconv.Itoa(imgid) + ext

}

func nameFromContentType(ct string) string {

	re := regexp.MustCompile(`\"(.+)\"`)
	sm := re.FindSubmatch([]byte(ct))
	if len(sm) > 1 {
		return string(sm[1])
	}
	return ct

}

func processImages(m Email, EntrantID int, BonusID string, EmailID uint32) (bool, int, string, time.Time) {

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
			photoid = writeImage(EntrantID, BonusID, EmailID, pix, string(a.Filename))
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
			photoid = writeImage(EntrantID, BonusID, EmailID, pix, a.ContentDisposition)
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

	return photosok, numphotos, photoids, photoTime

}

func timeFromPhoto(fname string, cd string) time.Time {

	// 20210717_185053.jpg
	nametimRE := regexp.MustCompile(`(\d{4})(\d\d)(\d\d)_(\d\d)(\d\d)(\d\d)`)
	ptime := time.Time{}
	xx := nametimRE.FindStringSubmatch(fname)

	if xx == nil {
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
		ptime, _ = time.Parse(time.RFC3339, xx[1]+"-"+xx[2]+"-"+xx[3]+"T"+xx[4]+":"+xx[5]+":"+xx[6]+cfg.OffsetTZ)
	}
	return ptime
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
