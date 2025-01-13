package main

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/chromedp/chromedp"
	_ "github.com/mattn/go-sqlite3"
)

// Scraper defines the structure for scraping configuration
type Scraper struct {
	UserAgents    []string
	HTTPClient    *http.Client
	Concurrency   int
	Sites         []string
	CustomParsers map[string]func(*goquery.Document) error
	DB            *sql.DB
}

// NewScraper initializes a new scraper
func NewScraper() *Scraper {
	// Initialize SQLite DB
	db, err := sql.Open("sqlite3", "./scraper_data.db")
	if err != nil {
		log.Fatalf("Error opening database: %s", err)
	}

	// Create a table for storing scraped data
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS scraped_data (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  site TEXT,
  data TEXT,
  timestamp DATETIME DEFAULT CURRENT_TIMESTAMP
 )`)
	if err != nil {
		log.Fatalf("Error creating table: %s", err)
	}

	return &Scraper{
		UserAgents: []string{
			"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36",
		},
		HTTPClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		Concurrency:   5,
		CustomParsers: make(map[string]func(*goquery.Document) error),
		DB:            db,
	}
}
func (s *Scraper) SetupDatabase() {
	_, err := s.DB.Exec(`
        CREATE TABLE IF NOT EXISTS word_counts (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            site TEXT,
            word TEXT,
            count INTEGER,
            timestamp DATETIME DEFAULT CURRENT_TIMESTAMP
        );
    `)
	if err != nil {
		log.Fatalf("Error creating database schema: %s", err)
	}
}

// FetchURL fetches a URL and returns the response body
func (s *Scraper) FetchURL(url string) (io.ReadCloser, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	// Set a random User-Agent
	req.Header.Set("User-Agent", s.UserAgents[time.Now().UnixNano()%int64(len(s.UserAgents))])

	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return resp.Body, nil
}

// ParseDynamicContent handles JavaScript-rendered pages
func (s *Scraper) ParseDynamicContent(url string) (string, error) {
	ctx, cancel := chromedp.NewContext(context.Background(), chromedp.WithLogf(log.Printf))
	timeoutCtx, timeoutCancel := context.WithTimeout(ctx, 30*time.Second)
	defer timeoutCancel()
	defer cancel()

	var html string
	err := chromedp.Run(timeoutCtx,
		chromedp.Navigate(url),
		chromedp.OuterHTML("html", &html),
	)
	if err != nil {
		return "", err
	}
	return html, nil
}

// ProcessAPI fetches and parses JSON from an API
func (s *Scraper) ProcessAPI(apiURL string) {
	resp, err := s.FetchURL(apiURL)
	if err != nil {
		log.Printf("Error fetching API URL %s: %s", apiURL, err)
		return
	}
	defer resp.Close()

	var jsonData []map[string]interface{}
	err = json.NewDecoder(resp).Decode(&jsonData)
	if err != nil {
		log.Printf("Error decoding JSON from API %s: %s", apiURL, err)
		return
	}

	// Example: Log the parsed data
	for _, item := range jsonData {
		log.Printf("Data from API %s: %+v", apiURL, item)
		// Save each item to the database
		s.saveData(apiURL, fmt.Sprintf("%+v", item))
	}
}

// saveData saves scraped data to the database
func (s *Scraper) saveData(site string, data string) {
	_, err := s.DB.Exec("INSERT INTO scraped_data (site, data) VALUES (?, ?)", site, data)
	if err != nil {
		log.Printf("Error saving data to database: %s", err)
	}
}

// ProcessSite processes a single site
func (s *Scraper) ProcessSite(url string) {
	log.Printf("Processing site: %s", url)

	var htmlContent io.ReadCloser
	var err error

	// Check if the site requires dynamic content handling
	if _, ok := s.CustomParsers[url]; ok {
		htmlString, dynamicErr := s.ParseDynamicContent(url)
		if dynamicErr != nil {
			log.Printf("Error fetching dynamic content: %s", dynamicErr)
			return
		}
		htmlContent = io.NopCloser(strings.NewReader(htmlString))
	} else {
		htmlContent, err = s.FetchURL(url)
		if err != nil {
			log.Printf("Error fetching URL %s: %s", url, err)
			return
		}
	}
	defer htmlContent.Close()

	doc, err := goquery.NewDocumentFromReader(htmlContent)
	if err != nil {
		log.Printf("Error parsing HTML for URL %s: %s", url, err)
		return
	}

	// Check if there's a custom parser for this site
	if parser, ok := s.CustomParsers[url]; ok {
		err := parser(doc)
		if err != nil {
			log.Printf("Error parsing site %s: %s", url, err)
		}
	} else {
		// Default processing
		doc.Find("a").Each(func(i int, sel *goquery.Selection) {
			link, exists := sel.Attr("href")
			if exists {
				log.Printf("Found link: %s", link)
				s.saveData(url, link)
			}
		})
	}
}

// Run starts the scraper with concurrency
func (s *Scraper) Run() {
	var wg sync.WaitGroup
	sem := make(chan struct{}, s.Concurrency)

	for _, site := range s.Sites {
		wg.Add(1)
		sem <- struct{}{}

		go func(site string) {
			defer wg.Done()
			s.ProcessSite(site)
			<-sem
		}(site)
	}

	wg.Wait()
}

func (s *Scraper) ExportWordCountsToCSVGrouped(filePath string) {
	file, err := os.Create(filePath)
	if err != nil {
		log.Fatalf("Error creating CSV file: %s", err)
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	defer writer.Flush()

	// Write CSV headers
	writer.Write([]string{"Site", "Words and Counts"})

	// Query data grouped by site
	rows, err := s.DB.Query("SELECT site, word, count FROM word_counts ORDER BY site")
	if err != nil {
		log.Fatalf("Error querying database: %s", err)
	}
	defer rows.Close()

	// Map to group results by site
	siteData := make(map[string]map[string]int)

	for rows.Next() {
		var site, word string
		var count int
		err := rows.Scan(&site, &word, &count)
		if err != nil {
			log.Printf("Error scanning row: %s", err)
			continue
		}

		// Group words by site
		if _, exists := siteData[site]; !exists {
			siteData[site] = make(map[string]int)
		}
		siteData[site][word] = count
	}

	// Write grouped data to the CSV
	for site, words := range siteData {
		var wordCounts []string
		for word, count := range words {
			wordCounts = append(wordCounts, fmt.Sprintf("%s: %d", word, count))
		}
		writer.Write([]string{site, strings.Join(wordCounts, " | ")})
	}

	log.Printf("Grouped data exported to %s", filePath)
}

func (s *Scraper) SearchWordInSite(url string, word string) {
	log.Printf("Searching for the word '%s' in site: %s", word, url)
	htmlContent, err := s.FetchURL(url)
	if err != nil {
		log.Printf("Error fetching URL %s: %s", url, err)
		return
	}
	defer htmlContent.Close()

	doc, err := goquery.NewDocumentFromReader(htmlContent)
	if err != nil {
		log.Printf("Error parsing HTML for URL %s: %s", url, err)
		return
	}

	// Search for the specific word in the text content
	foundInstances := 0
	doc.Find("body").Each(func(i int, sel *goquery.Selection) {
		text := sel.Text()
		if occurrences := countWordOccurrences(text, word); occurrences > 0 {
			foundInstances += occurrences
		}
	})

	log.Printf("Found '%s' %d times in %s", word, foundInstances, url)

	// Save the count to the database
	_, err = s.DB.Exec("INSERT INTO word_counts (site, word, count) VALUES (?, ?, ?)", url, word, foundInstances)
	if err != nil {
		log.Printf("Error saving word count for site %s: %s", url, err)
	}
}

// Utility function to count word occurrences
func countWordOccurrences(text, word string) int {
	return strings.Count(strings.ToLower(text), strings.ToLower(word))
}

func (s *Scraper) ClearWordCountsTable() {
	_, err := s.DB.Exec("DELETE FROM word_counts")
	if err != nil {
		log.Printf("Error clearing word_counts table: %s", err)
	} else {
		log.Println("Cleared word_counts table.")
	}
}

func main() {
	// Define the clear flag
	clearTable := flag.Bool("clear", false, "Clear the word_counts table before starting")
	flag.Parse()

	scraper := NewScraper()

	// Ensure tables are created
	scraper.SetupDatabase()

	// Clear the table if the flag is set
	if *clearTable {
		scraper.ClearWordCountsTable()
	}

	// Add target sites
	scraper.Sites = []string{
		"https://www.technologyreview.com/2024/08/30/1103385/a-new-way-to-build-neural-networks-could-make-ai-more-understandable/",
		"https://eng.vt.edu/magazine/stories/fall-2023/ai.html",
		"https://otus.ru/nest/post/1263/",
		"https://blog.productstar.ru/kak-rabotayut-nejronnye-seti/",
		"https://naked-science.ru/article/column/stabilnost-binarnyh-nejro",
		"https://naked-science.ru/article/column/rezhimy-raboty-elektrodvi",
		"https://naked-science.ru/article/column/kartinok-s-pomoshhyu-nejr",
		"https://naked-science.ru/article/column/karbonitridov-s-pomoshhyu",
		"https://naked-science.ru/article/column/seti-svyazannye-s-depress",
		"https://naked-science.ru/article/physics/novaya-arhitektura-optich",
		"https://naked-science.ru/article/interview/yandex-research",
		"https://naked-science.ru/article/column/kompyuternoelyat-bole",
		"https://naked-science.ru/article/column/v-niu-vse-nashli-sposob-r",
		"https://naked-science.ru/article/column/predlozhen-sposob-udeshevleniya",
		"https://naked-science.ru/article/chemistry/razrabotana-samoupravlyae",
		"http://synergy-journal.ru/archive/article5125",
		"https://viasite.ru/articles/overview/neural_network_artificial_intelligence/",
		"https://www.kommersant.ru/doc/3495930",
		"https://proglib.io/p/nauchnye-stati-po-ii-kotorye-stoit-prochitat-v-2020-godu-2020-10-31",
		"https://www.simbirsoft.com/blog/tri-metoda-vizualnoy-interpretatsii-svertochnykh-neyronnykh-setey/",
		"https://1-sept.ru/component/djclassifieds/?view=item&cid=4:publ-ssh-bf&id=2759:%D0%BF%D1%80%D0%B0%D0%BA%D1%82%D0%B8%D1%87%D0%B5%D1%81%D0%BA%D0%BE%D0%B5-%D0%BF%D1%80%D0%B8%D0%BC%D0%B5%D0%BD%D0%B5%D0%BD%D0%B8%D0%B5-%D0%BD%D0%B5%D0%B9%D1%80%D0%BE%D0%BD%D0%BD%D1%8B%D1%85-%D1%81%D0%B5%D1%82%D0%B5%D0%B9-%D0%B2-%D0%BE%D0%B1%D1%80%D0%B0%D0%B7%D0%BE%D0%B2%D0%B0%D0%BD%D0%B8%D0%B8-%D0%B8-%D1%83%D1%87%D0%B5%D0%B1%D0%BD%D0%BE%D0%BC-%D0%BF%D1%80%D0%BE%D1%86%D0%B5%D1%81%D1%81%D0%B5&Itemid=464",
		"https://dzen.ru/a/Xbp256P25ACxy6JB",
		"https://uxi.run/blog/ispolzovanie-neyronnykh-setey-dlya-raboty-s-kontentom-v-sotsialnykh-setyakh/",
		"https://tproger.ru/articles/kakim-budet-budushhee-nejrosetej-v-2024-godu",
		"https://gb.ru/blog/neironnye-seti/",
		"https://k-telecom.org/articles/luchshij-drug-ili-ugroza-chelovechestvu-chto-takoe-nejroseti-kak-ih-ispolzovat-i-chego-zhdat-ot-nejronok/",
		"http://www.neuropro.ru/papers.shtml",
		"https://moluch.ru/archive/138/38781/",
		"https://core.ac.uk/download/pdf/84594131.pdf",
		"https://www.tadviser.ru/index.php/%D0%A1%D1%82%D0%B0%D1%82%D1%8C%D1%8F:%D0%9D%D0%B5%D0%B9%D1%80%D0%BE%D1%81%D0%B5%D1%82%D0%B8_(%D0%BD%D0%B5%D0%B9%D1%80%D0%BE%D0%BD%D0%BD%D1%8B%D0%B5_%D1%81%D0%B5%D1%82%D0%B8)",
		"https://neerc.ifmo.ru/wiki/index.php?title=%D0%9D%D0%B5%D0%B9%D1%80%D0%BE%D0%BD%D0%BD%D1%8B%D0%B5_%D1%81%D0%B5%D1%82%D0%B8,_%D0%BF%D0%B5%D1%80%D1%86%D0%B5%D0%BF%D1%82%D1%80%D0%BE%D0%BD",
		"https://habr.com/ru/articles/751340/",
	}

	// Search for specific words
	wordsToSearch := []string{"нейро", "недос"}
	for _, site := range scraper.Sites {
		for _, word := range wordsToSearch {
			scraper.SearchWordInSite(site, word)
		}
	}

	// Export results to a CSV file
	scraper.ExportWordCountsToCSVGrouped("word_counts_grouped.csv")
}
