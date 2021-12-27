package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dghubble/go-twitter/twitter"
	"github.com/dghubble/oauth1"
	"github.com/knieriem/odf/ods"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"golang.org/x/text/number"
)

var (
	telegramAPIToken      = os.Getenv("TELEGRAM_API_TOKEN")
	updatesTelegramChatID = os.Getenv("UPDATES_TELEGRAM_CHAT_ID")

	twitterConsumerKey    = os.Getenv("TWITTER_CONSUMER_KEY")
	twitterConsumerSecret = os.Getenv("TWITTER_CONSUMER_SECRET")
	twitterAccessToken    = os.Getenv("TWITTER_ACCESS_TOKEN")
	twitterAccessSecret   = os.Getenv("TWITTER_ACCESS_SECRET")
)

func main() {
	err := scrap()
	if err != nil {
		log.Printf("Error scraping: %s", err)
		os.Exit(1)
	}
}

func scrap() (err error) {
	dir := os.DirFS("reports/vaccination")
	names, err := fs.Glob(dir, "Informe_Comunicacion_*.ods")
	if err != nil {
		return fmt.Errorf("listing ODSs from reports dir: %w", err)
	}
	sort.Strings(names)

	lastName := ""
	if len(names) > 0 {
		lastName = names[len(names)-1]
	}

	nextName, err := fetchCurrentName()
	if err != nil {
		return fmt.Errorf("fetching current report name: %w", err)
	}
	if nextName == lastName {
		log.Printf("No new report yet. Still %s.", nextName)
		return nil
	}

	nextContents, ok, err := fetchReport(nextName)
	if err != nil {
		return fmt.Errorf("fetching contents: %w", err)
	}
	if !ok {
		log.Printf("No report yet: %s", nextName)
		return nil
	}

	if lastName == "" {
		lastName = nextName

		err = os.WriteFile("reports/vaccination/"+lastName, nextContents, 0644)
		if err != nil {
			return fmt.Errorf("creating %s: %w", lastName, err)
		}
	}

	lastContents, err := fs.ReadFile(dir, lastName)
	if err != nil {
		return fmt.Errorf("reading last report: %w", err)
	}

	baseCfg := extractConfig{
		totalRow: 22,
	}

	var lastReport, nextReport vaccReport
	for _, c := range []struct {
		contents []byte
		report   *vaccReport
		cfg      extractConfig
	}{
		{lastContents, &lastReport, baseCfg},
		{nextContents, &nextReport, baseCfg},
	} {
		odfile, err := ods.NewReader(bytes.NewReader(c.contents), int64(len(c.contents)))
		if err != nil {
			return fmt.Errorf("reading ODF: %w", err)
		}
		var doc ods.Doc
		err = odfile.ParseContent(&doc)
		if err != nil {
			return fmt.Errorf("parsing ODS: %w", err)
		}
		c.cfg.extractReport(&doc, c.report)
	}

	log.Println("Handling update:", nextName)

	err = postToTelegram(&lastReport, &nextReport)
	if err != nil {
		return fmt.Errorf("posting to Telegram: %w", err)
	}

	err = postToTwitter(&lastReport, &nextReport)
	if err != nil {
		return fmt.Errorf("posting to Twitter: %w", err)
	}

	err = os.WriteFile("reports/vaccination/"+nextName, nextContents, 0644)
	if err != nil {
		return fmt.Errorf("creating %s: %w", nextName, err)
	}

	log.Println("Update handled:", nextName)

	return nil
}

var reportNameRgx = regexp.MustCompile("documentos/(Informe_Comunicacion_[0-9]{8}.ods)")

func fetchCurrentName() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", "https://www.mscbs.gob.es/profesionales/saludPublica/ccayes/alertasActual/nCov/vacunaCovid19.htm", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	m := reportNameRgx.FindSubmatch(body)
	if len(m) != 2 {
		return "", fmt.Errorf("no link to report found in HTML")
	}
	return string(m[1]), nil
}

func fetchReport(name string) ([]byte, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", "https://www.mscbs.gob.es/profesionales/saludPublica/ccayes/alertasActual/nCov/documentos/"+name, nil)
	if err != nil {
		return nil, false, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, false, nil
	}

	contents, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, err
	}

	return contents, true, nil
}

type extractConfig struct {
	totalRow int
}

func (cfg extractConfig) extractReport(doc *ods.Doc, report *vaccReport) error {
	{
		totalTable := doc.Table[0].Strings()

		header := totalTable[0]
		assert(header[8] == "Nº Personas con al menos 1 dosis")
		assert(header[9] == "Nº Personas vacunadas\n(pauta completada)")
		assert(header[5] == "Total Dosis entregadas (1)")
		assert(header[6] == "Dosis administradas (2)")

		totals := totalTable[cfg.totalRow]
		assert(totals[0] == "Totales")

		report.TotalVacced.Single = parseInt(totals[8])
		report.TotalVacced.Full = parseInt(totals[9])

		report.Doses.Available = parseInt(totals[5])
		report.Doses.Given = parseInt(totals[6])
	}

	tableOffset := 0
	singleTable := doc.Table[2+tableOffset].Strings()
	fullTable := doc.Table[3+tableOffset].Strings()

	totalPopCol := 18
	assert(singleTable[0][totalPopCol] == "Total Población INE Población a Vacunar (1)")
	assert(fullTable[0][totalPopCol] == "Total Población INE Población a Vacunar (1)")
	report.TotalVacced.PopSize = 47_431_256 // INE 2020

	for i, group := range []struct {
		v       *Vacced
		popSize int
	}{
		{&report.VaccedByAge._80Plus, 2_834_024},
		{&report.VaccedByAge._70_79, 3_960_045},
		{&report.VaccedByAge._60_69, 5_336_986},
		{&report.VaccedByAge._50_59, 7_033_306},
		{&report.VaccedByAge._40_49, 7_891_737},
		{&report.VaccedByAge._30_39, 6_230_403},
		{&report.VaccedByAge._20_29, 4_944_640},
		{&report.VaccedByAge._12_19, 3_888_686},
	} {
		width := 2
		i := i * width
		group.v.Single = parseInt(singleTable[cfg.totalRow][i+1])
		group.v.Full = parseInt(fullTable[cfg.totalRow][i+1])
		group.v.PopSize = group.popSize
	}

	return nil
}

type vaccReport struct {
	Doses struct {
		Available int
		Given     int
	}
	TotalVacced Vacced
	VaccedByAge VaccedByAge
}

type VaccedByAge struct {
	_80Plus Vacced
	_70_79  Vacced
	_60_69  Vacced
	_50_59  Vacced
	_40_49  Vacced
	_30_39  Vacced
	_20_29  Vacced
	_12_19  Vacced
}

func (v VaccedByAge) Total() Vacced {
	var t Vacced
	for _, v := range []Vacced{
		v._80Plus,
		v._70_79,
		v._60_69,
		v._50_59,
		v._40_49,
		v._30_39,
		v._20_29,
		v._12_19,
	} {
		t.PopSize += v.PopSize
		t.Single += v.Single
		t.Full += v.Full
	}
	return t
}

func (v VaccedByAge) MaxPopSize() int {
	var max int
	for _, v := range []Vacced{
		v._80Plus,
		v._70_79,
		v._60_69,
		v._50_59,
		v._40_49,
		v._30_39,
		v._20_29,
		v._12_19,
	} {
		if v.PopSize > max {
			max = v.PopSize
		}
	}
	return max
}

type Vacced struct {
	PopSize int
	Single  int
	Full    int
}

func (d Vacced) Pct() struct {
	Single float64
	Full   float64
} {
	return struct {
		Single float64
		Full   float64
	}{
		intPct(d.Single, d.PopSize),
		intPct(d.Full, d.PopSize),
	}
}

func postToTelegram(lastReport, nextReport *vaccReport) error {
	var msg strings.Builder

	lastPct := lastReport.TotalVacced.Pct()
	nextPct := nextReport.TotalVacced.Pct()

	fmt.Fprintf(&msg, "<pre>%s</pre>\n", progressBar(
		25,
		nextPct.Full,
		nextPct.Single-nextPct.Full,
	))
	fmt.Fprintf(&msg, "<strong>💉💉 %s | 💉 %s</strong>\n",
		fmtPct(nextPct.Full, 1),
		fmtPct(nextPct.Single, 1),
	)

	fmt.Fprintln(&msg)

	fmt.Fprintf(&msg, "Dosis puestas: <strong>%s</strong> | Total: %s\n\n",
		fmtIncr(fmtFloat(float64(nextReport.Doses.Given-lastReport.Doses.Given), 1)),
		fmtFloat(float64(nextReport.Doses.Given), 3),
	)

	fmt.Fprintln(&msg)

	fmt.Fprintf(&msg, "<strong>💉💉 Pauta completa</strong>\n<strong>%s</strong> (%s pob.)\nTotal: <strong>%s</strong> (%s pob.)\n\n",
		fmtIncr(fmtFloat(float64(nextReport.TotalVacced.Full-lastReport.TotalVacced.Full), 1)),
		fmtIncr(fmtPct(nextPct.Full-lastPct.Full, 1)),
		fmtFloat(float64(nextReport.TotalVacced.Full), 3),
		fmtPct(nextPct.Full, 1),
	)
	fmt.Fprintf(&msg, "<strong>💉 Al menos una dosis</strong>\n<strong>%s</strong> (%s pob.)\nTotal: <strong>%s</strong> (%s pob.)\n\n",
		fmtIncr(fmtFloat(float64(nextReport.TotalVacced.Single-lastReport.TotalVacced.Single), 1)),
		fmtIncr(fmtPct(nextPct.Single-lastPct.Single, 1)),
		fmtFloat(float64(nextReport.TotalVacced.Single), 3),
		fmtPct(nextPct.Single, 1),
	)

	fmt.Fprintf(&msg, "\n%% por grupos de edad (💉💉 completa / 💉 al menos una dosis):\n\n")

	for _, c := range []struct {
		title string
		v     Vacced
	}{
		{"≥80  ", nextReport.VaccedByAge._80Plus},
		{"70-79", nextReport.VaccedByAge._70_79},
		{"60-69", nextReport.VaccedByAge._60_69},
		{"50-59", nextReport.VaccedByAge._50_59},
		{"40-49", nextReport.VaccedByAge._40_49},
		{"30-39", nextReport.VaccedByAge._30_39},
		{"20-29", nextReport.VaccedByAge._20_29},
		{"12-19", nextReport.VaccedByAge._12_19},
	} {
		pct := c.v.Pct()

		const maxWidth = 20
		ageWidth := int(math.Round(
			float64(c.v.PopSize*maxWidth) /
				float64(nextReport.VaccedByAge.MaxPopSize()),
		))

		fmt.Fprintf(&msg, "<pre>%s %s%s (%s / %s)</pre>\n",
			c.title,
			progressBar(ageWidth, pct.Full, pct.Single-pct.Full),
			strings.Repeat(" ", maxWidth-ageWidth),
			fmtPct(pct.Full, 1),
			fmtPct(pct.Single, 1),
		)
	}

	fmt.Fprintln(&msg)
	fmt.Fprintln(&msg, `Informe completo disponible en <a href="https://www.mscbs.gob.es/profesionales/saludPublica/ccayes/alertasActual/nCov/vacunaCovid19.htm">la web del Ministerio de Sanidad</a>.`)

	return sendTelegramMessage(map[string]interface{}{
		"chat_id":    updatesTelegramChatID,
		"text":       msg.String(),
		"parse_mode": "HTML",
	})
}

func postToTwitter(lastReport, nextReport *vaccReport) error {
	var tweets []string
	var msg strings.Builder

	lastPct := lastReport.TotalVacced.Pct()
	nextPct := nextReport.TotalVacced.Pct()

	fmt.Fprintf(&msg, "%s\n", progressBar(
		20,
		nextPct.Full,
		nextPct.Single-nextPct.Full,
	))
	fmt.Fprintf(&msg, "💉💉 %s | 💉 %s\n",
		fmtPct(nextPct.Full, 1),
		fmtPct(nextPct.Single, 1),
	)

	fmt.Fprintln(&msg)

	fmt.Fprintf(&msg, "Puestas: %s | Total: %s\n\n",
		fmtIncr(fmtFloat(float64(nextReport.Doses.Given-lastReport.Doses.Given), 1)),
		fmtFloat(float64(nextReport.Doses.Given), 3),
	)
	fmt.Fprintf(&msg, "💉💉 Pauta completa\n%s (%s pob.)\nTotal: %s (%s pob.)\n\n",
		fmtIncr(fmtFloat(float64(nextReport.TotalVacced.Full-lastReport.TotalVacced.Full), 1)),
		fmtIncr(fmtPct(nextPct.Full-lastPct.Full, 1)),
		fmtFloat(float64(nextReport.TotalVacced.Full), 3),
		fmtPct(nextPct.Full, 1),
	)
	fmt.Fprintf(&msg, "💉 Al menos una dosis\n%s (%s pob.)\nTotal: %s (%s pob.)",
		fmtIncr(fmtFloat(float64(nextReport.TotalVacced.Single-lastReport.TotalVacced.Single), 1)),
		fmtIncr(fmtPct(nextPct.Single-lastPct.Single, 1)),
		fmtFloat(float64(nextReport.TotalVacced.Single), 3),
		fmtPct(nextPct.Single, 1),
	)

	tweets = append(tweets, msg.String())
	msg = strings.Builder{}

	fmt.Fprintf(&msg, "Por edad (💉💉/💉 %%):\n\n")

	for _, c := range []struct {
		title string
		v     Vacced
	}{
		{"≥80", nextReport.VaccedByAge._80Plus},
		{"7x", nextReport.VaccedByAge._70_79},
		{"6x", nextReport.VaccedByAge._60_69},
		{"5x", nextReport.VaccedByAge._50_59},
		{"4x", nextReport.VaccedByAge._40_49},
		{"3x", nextReport.VaccedByAge._30_39},
		{"2x", nextReport.VaccedByAge._20_29},
		{"12-19", nextReport.VaccedByAge._12_19},
	} {
		pct := c.v.Pct()
		fmt.Fprintf(&msg, "%s %s %s/%s\n",
			progressBar(10, pct.Full, pct.Single-pct.Full),
			c.title,
			fmtFloat(pct.Full, 0),
			fmtFloat(pct.Single, 0),
		)
	}

	tweets = append(tweets, msg.String())

	err := tweetThread(tweets...)
	if err != nil {
		return err
	}
	return nil
}

func tweetThread(msgs ...string) error {
	var lastTweet *twitter.Tweet
	for i, msg := range msgs {
		if twitterClient == nil {
			fmt.Println("tweet: ------\n" + msg + "\n------")
			continue
		}

		var params *twitter.StatusUpdateParams
		if lastTweet != nil {
			params = &twitter.StatusUpdateParams{
				InReplyToStatusID: lastTweet.ID,
			}
		}
		t, resp, err := twitterClient.Statuses.Update(msg, params)
		if err != nil {
			return fmt.Errorf("posting tweet #%d: %w", i, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("posting tweet #%d: status %d; body: %s", i, resp.StatusCode, body)
		}
		lastTweet = t
	}
	return nil
}

func sendTelegramMessage(msg interface{}) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if telegramAPIToken == "" {
		fmt.Println("telegram: ------\n" + msg.(map[string]interface{})["text"].(string) + "\n------")
		return nil
	}

	body, err := json.Marshal(msg)
	if err != nil {
		panic(err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.telegram.org/bot"+telegramAPIToken+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		panic(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var telegramResp struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	err = json.NewDecoder(resp.Body).Decode(&telegramResp)
	if err != nil {
		return fmt.Errorf("decoding response from Telegram: %w", err)
	}
	if !telegramResp.OK {
		return fmt.Errorf("from Telegram: %s", telegramResp.Description)
	}

	return nil
}

func assert(ok bool) {
	if !ok {
		_, file, line, _ := runtime.Caller(1)
		panic(fmt.Errorf("assertion failed at %s:%d", file, line))
	}
}

func parseInt(s string) int {
	v, err := strconv.Atoi(strings.ReplaceAll(s, ".", ""))
	if err != nil {
		panic(err)
	}
	return v
}

func intPct(n, base int) float64 {
	return float64(n) * 100 / float64(base)
}

func progressBar(width int, pcts ...float64) string {
	s := float64(width) / 100.0

	var bar strings.Builder
	var cells int
	var pctDone float64

	for i, pct := range pcts {
		pctDone += pct
		pctCells := int(math.Round(pctDone*s)) - cells

		cells += pctCells
		if cells > width {
			pctCells -= cells - width
			cells = width
		}

		c := "▓"
		if i%2 == 1 {
			c = "▒"
		}
		bar.WriteString(strings.Repeat(c, pctCells))
	}

	rest := width - cells
	bar.WriteString(strings.Repeat("░", rest))

	return bar.String()
}

var twitterClient = func() *twitter.Client {
	if twitterConsumerKey == "" {
		return nil
	}
	return twitter.NewClient(
		oauth1.NewConfig(
			twitterConsumerKey,
			twitterConsumerSecret,
		).Client(
			oauth1.NoContext,
			oauth1.NewToken(twitterAccessToken, twitterAccessSecret),
		),
	)
}()

var fmtFloat, fmtPct, fmtIncr = func() (
	func(float64, int) string,
	func(float64, int) string,
	func(string) string,
) {
	p := message.NewPrinter(language.Spanish)
	units := [...]string{"", " k", " M", " G"}

	fmtFloat := func(f float64, maxFrac int) string {
		var i int
		for i = 0; f >= 1000 && i < len(units); i++ {
			f /= 1000
		}
		return p.Sprintf("%v%s", number.Decimal(f, number.MaxFractionDigits(maxFrac)), units[i])
	}

	fmtPct := func(pct float64, maxFrac int) string {
		return p.Sprint(number.Percent(pct/100, number.MaxFractionDigits(maxFrac)))
	}

	fmtIncr := func(s string) string {
		if s[0] != '-' {
			s = "+" + s
		}
		return s
	}

	return fmtFloat, fmtPct, fmtIncr
}()
