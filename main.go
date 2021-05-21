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

	"github.com/knieriem/odf/ods"
)

var (
	telegramAPIToken             = os.Getenv("TELEGRAM_API_TOKEN")
	updatesTelegramChatID        = os.Getenv("UPDATES_TELEGRAM_CHAT_ID")
	twitterUpdatesTelegramChatID = os.Getenv("TWITTER_UPDATES_TELEGRAM_CHAT_ID")
)

func main() {
	err := scrap()
	if err != nil {
		log.Printf("Error scraping: %s", err)
	}
}

func scrap() (err error) {
	dir := os.DirFS("reports/vaccination")
	names, err := fs.Glob(dir, "Informe_Comunicacion_*.ods")
	if err != nil {
		return fmt.Errorf("listing ODSs from reports dir: %w", err)
	}
	sort.Strings(names)

	lastName := names[len(names)-1]

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

	lastContents, err := fs.ReadFile(dir, lastName)
	if err != nil {
		return fmt.Errorf("reading last report: %w", err)
	}

	var lastReport, nextReport vaccReport
	for _, c := range []struct {
		contents []byte
		report   *vaccReport
	}{
		{lastContents, &lastReport},
		{nextContents, &nextReport},
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
		extractReport(&doc, c.report)
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

func extractReport(doc *ods.Doc, report *vaccReport) error {
	{
		totalTable := doc.Table[0].Strings()

		header := totalTable[0]
		assert(header[8] == "Nº Personas con al menos 1 dosis")
		assert(header[9] == "Nº Personas vacunadas\n(pauta completada)")
		assert(header[5] == "Total Dosis entregadas (1)")
		assert(header[6] == "Dosis administradas (2)")

		totals := totalTable[21]
		assert(totals[0] == "Totales")

		report.TotalVacced.Single = parseInt(totals[8])
		report.TotalVacced.Full = parseInt(totals[9])

		report.Doses.Available = parseInt(totals[5])
		report.Doses.Given = parseInt(totals[6])
	}

	singleTable := doc.Table[3].Strings()
	fullTable := doc.Table[4].Strings()

	assert(singleTable[0][23] == "Total Población INE Población a Vacunar (1)")
	assert(fullTable[0][23] == "Total Población INE Población a Vacunar (1)")
	report.TotalVacced.PopSize = parseInt(singleTable[21][23])

	for i, group := range []*Vacced{
		&report.VaccedByAge._80Plus,
		&report.VaccedByAge._70_79,
		&report.VaccedByAge._60_69,
		&report.VaccedByAge._50_59,
		&report.VaccedByAge._25_49,
		&report.VaccedByAge._18_24,
		&report.VaccedByAge._16_17,
	} {
		i := i * 3
		group.Single = parseInt(singleTable[21][i+1])
		group.Full = parseInt(fullTable[21][i+1])
		group.PopSize = parseInt(singleTable[21][i+2])
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
	_25_49  Vacced
	_18_24  Vacced
	_16_17  Vacced
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

	fmt.Fprintf(&msg, "<pre>%s</pre>\n", progressBar(nextPct.Full, nextPct.Single-nextPct.Full, 25))

	fmt.Fprintf(&msg, "\nPauta completa: <strong>%0.2f %%</strong> (<strong>%+0.1f k;</strong> %+0.2f %%)\n",
		nextPct.Full,
		float64(nextReport.TotalVacced.Full-lastReport.TotalVacced.Full)/1000,
		nextPct.Full-lastPct.Full,
	)
	fmt.Fprintf(&msg, "Al menos una dosis: %0.2f %% (%+0.1f k; %+0.2f %%)\n",
		nextPct.Single,
		float64(nextReport.TotalVacced.Single-lastReport.TotalVacced.Single)/1000,
		nextPct.Single-lastPct.Single,
	)

	fmt.Fprintf(&msg, "\nDosis:\n\n")

	fmt.Fprintf(&msg, "Entregadas: %0.3f M (%+0.1f k)\n",
		float64(nextReport.Doses.Available)/1000000,
		float64(nextReport.Doses.Available-lastReport.Doses.Available)/1000,
	)
	fmt.Fprintf(&msg, "Puestas: %0.3f M (<strong>%+0.1f k;</strong> %0.2f %% de entregadas)\n",
		float64(nextReport.Doses.Given)/1000000,
		float64(nextReport.Doses.Given-lastReport.Doses.Given)/1000,
		intPct(nextReport.Doses.Given, nextReport.Doses.Available),
	)

	fmt.Fprintf(&msg, "\nPor grupos de edad (una dosis / completa):\n\n")

	for _, c := range []struct {
		title string
		v     Vacced
	}{
		{"≥80  ", nextReport.VaccedByAge._80Plus},
		{"70-79", nextReport.VaccedByAge._70_79},
		{"60-69", nextReport.VaccedByAge._60_69},
		{"50-59", nextReport.VaccedByAge._50_59},
		{"25-49", nextReport.VaccedByAge._25_49},
		{"18-24", nextReport.VaccedByAge._18_24},
		{"16-27", nextReport.VaccedByAge._16_17},
	} {
		pct := c.v.Pct()
		fmt.Fprintf(&msg, "<pre>%s %s (%0.2f %% / %0.2f %%)</pre>\n",
			c.title,
			progressBar(pct.Full, pct.Single-pct.Full, 20),
			pct.Single,
			pct.Full,
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
	var msg strings.Builder

	lastPct := lastReport.TotalVacced.Pct()
	nextPct := nextReport.TotalVacced.Pct()

	fmt.Fprintf(&msg, "%s\n", progressBar(nextPct.Full, nextPct.Single-nextPct.Full, 30))

	fmt.Fprintf(&msg, "\nPauta completa: %0.2f %% (%+0.1f k; %+0.2f %%)\n",
		nextPct.Full,
		float64(nextReport.TotalVacced.Full-lastReport.TotalVacced.Full)/1000,
		nextPct.Full-lastPct.Full,
	)
	fmt.Fprintf(&msg, "Al menos una dosis: %0.2f %% (%+0.1f k; %+0.2f %%)\n",
		nextPct.Single,
		float64(nextReport.TotalVacced.Single-lastReport.TotalVacced.Single)/1000,
		nextPct.Single-lastPct.Single,
	)

	fmt.Fprintf(&msg, "\nDosis:\n\n")

	fmt.Fprintf(&msg, "Entregadas: %0.3f M (%+0.1f k)\n",
		float64(nextReport.Doses.Available)/1000000,
		float64(nextReport.Doses.Available-lastReport.Doses.Available)/1000,
	)
	fmt.Fprintf(&msg, "Puestas: %0.3f M (%+0.1f k; %0.2f %% de entregadas)\n",
		float64(nextReport.Doses.Given)/1000000,
		float64(nextReport.Doses.Given-lastReport.Doses.Given)/1000,
		intPct(nextReport.Doses.Given, nextReport.Doses.Available),
	)

	// TODO: Actually post to Twitter. Setting up a Twitter bot is so annoying...

	err := sendTelegramMessage(map[string]interface{}{
		"chat_id": twitterUpdatesTelegramChatID,
		"text":    msg.String(),
	})
	if err != nil {
		return err
	}

	msg = strings.Builder{}

	fmt.Fprintf(&msg, "%% por edad:\n\n")

	for _, c := range []struct {
		title string
		v     Vacced
	}{
		{"≥80", nextReport.VaccedByAge._80Plus},
		{"7x", nextReport.VaccedByAge._70_79},
		{"6x", nextReport.VaccedByAge._60_69},
		{"5x", nextReport.VaccedByAge._50_59},
		{"25-49", nextReport.VaccedByAge._25_49},
		{"18-24", nextReport.VaccedByAge._18_24},
		{"16-17", nextReport.VaccedByAge._16_17},
	} {
		pct := c.v.Pct()
		fmt.Fprintf(&msg, "%s %s (%0.0f/%0.0f %%)\n",
			progressBar(pct.Full, pct.Single-pct.Full, 10),
			c.title,
			pct.Full,
			pct.Single,
		)
	}

	err = sendTelegramMessage(map[string]interface{}{
		"chat_id": twitterUpdatesTelegramChatID,
		"text":    msg.String(),
	})
	if err != nil {
		return err
	}

	msg = strings.Builder{}

	fmt.Fprintln(&msg, `Informe completo disponible en la web del Ministerio de Sanidad: https://www.mscbs.gob.es/profesionales/saludPublica/ccayes/alertasActual/nCov/vacunaCovid19.htm`)

	err = sendTelegramMessage(map[string]interface{}{
		"chat_id": twitterUpdatesTelegramChatID,
		"text":    msg.String(),
	})
	if err != nil {
		return err
	}

	return nil
}

func sendTelegramMessage(msg interface{}) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

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

func progressBar(strong, weak float64, width int) string {
	s := float64(width) / 100.0
	strongCells := int(math.Round(strong * s))
	weakCells := int(math.Round(weak * s))
	if weakCells < 0 || strongCells+weakCells > width {
		weakCells = 0
	}
	rest := width - strongCells - weakCells
	if rest < 0 {
		rest = 0
	}
	return fmt.Sprintf("%s%s%s",
		strings.Repeat("▓", strongCells),
		strings.Repeat("▒", weakCells),
		strings.Repeat("░", rest),
	)
}
