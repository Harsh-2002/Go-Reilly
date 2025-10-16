package oreilly

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"goreilly/internal/models"
	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/publicsuffix"
)

const (
	OrlyBaseHost   = "oreilly.com"
	SafariBaseHost = "learning." + OrlyBaseHost
	APIOriginHost  = "api." + OrlyBaseHost
	SafariBaseURL  = "https://" + SafariBaseHost
	ProfileURL     = SafariBaseURL + "/profile/"
	tmpBooksDir    = "/tmp/goreilly/books"
)

// Client handles O'Reilly book downloads
type Client struct {
	httpClient       *http.Client
	bookID           string
	bookInfo         *models.BookInfo
	chapters         []models.Chapter
	bookPath         string
	cssFiles         []string
	imageFiles       []string
	coverImage       string
	progressCallback models.ProgressCallback
	mu               sync.Mutex // Protects shared slices during concurrent access
}

// NewClient creates a new O'Reilly client
func NewClient(bookID string, cookiesPath string, callback models.ProgressCallback) (*Client, error) {
	log.Printf("[O'Reilly] Creating new client for book ID: %s", bookID)
	
	// Load cookies
	log.Printf("[O'Reilly] Loading cookies from: %s", cookiesPath)
	cookies, err := loadCookies(cookiesPath)
	if err != nil {
		log.Printf("[O'Reilly] ERROR: Failed to load cookies: %v", err)
		return nil, fmt.Errorf("failed to load cookies: %w", err)
	}
	log.Printf("[O'Reilly] Successfully loaded %d cookies", len(cookies))

	// Create cookie jar
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		log.Printf("[O'Reilly] ERROR: Failed to create cookie jar: %v", err)
		return nil, err
	}

	// Set cookies
	u, _ := url.Parse(SafariBaseURL)
	jar.SetCookies(u, cookies)

	client := &Client{
		httpClient: &http.Client{
			Jar:     jar,
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		bookID:           bookID,
		cssFiles:         []string{},
		imageFiles:       []string{},
		progressCallback: callback,
	}

	// Check authentication
	log.Printf("[O'Reilly] Checking authentication...")
	if err := client.checkLogin(); err != nil {
		log.Printf("[O'Reilly] ERROR: Authentication failed: %v", err)
		return nil, err
	}
	log.Printf("[O'Reilly] Authentication successful")

	return client, nil
}

// loadCookies loads cookies from JSON file
func loadCookies(path string) ([]*http.Cookie, error) {
	// Check multiple locations
	cookiePaths := []string{path, "/config/cookies.json", "./cookies.json", "../cookies.json"}
	
	var data []byte
	var err error
	
	for _, p := range cookiePaths {
		data, err = os.ReadFile(p)
		if err == nil {
			break
		}
	}
	
	if err != nil {
		return nil, fmt.Errorf("cookies.json not found")
	}

	var cookieMap map[string]string
	if err := json.Unmarshal(data, &cookieMap); err != nil {
		return nil, err
	}

	var cookies []*http.Cookie
	for name, value := range cookieMap {
		cookies = append(cookies, &http.Cookie{
			Name:   name,
			Value:  value,
			Domain: ".oreilly.com",
		})
	}

	return cookies, nil
}

// checkLogin verifies authentication
func (c *Client) checkLogin() error {
	resp, err := c.httpClient.Get(ProfileURL)
	if err != nil {
		return fmt.Errorf("unable to reach O'Reilly: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("authentication failed, please refresh cookies.json")
	}

	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), `user_type":"Expired"`) {
		return fmt.Errorf("account subscription expired")
	}

	return nil
}

// updateProgress calls the progress callback
func (c *Client) updateProgress(stage string, progress int, message string) {
	if c.progressCallback != nil {
		c.progressCallback(stage, progress, message)
	}
}

// GetBookInfo fetches book metadata
func (c *Client) GetBookInfo() error {
	c.updateProgress("info", 15, "Retrieving book info...")
	log.Printf("[O'Reilly] Fetching book info for ID: %s", c.bookID)

	apiURL := fmt.Sprintf("%s/api/v1/book/%s/", SafariBaseURL, c.bookID)
	resp, err := c.httpClient.Get(apiURL)
	if err != nil {
		log.Printf("[O'Reilly] ERROR: Failed to retrieve book info: %v", err)
		return fmt.Errorf("failed to retrieve book info: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Printf("[O'Reilly] ERROR: Book not found, status code: %d", resp.StatusCode)
		return fmt.Errorf("book not found or API error (status: %d)", resp.StatusCode)
	}

	var bookInfo models.BookInfo
	if err := json.NewDecoder(resp.Body).Decode(&bookInfo); err != nil {
		log.Printf("[O'Reilly] ERROR: Failed to parse book info: %v", err)
		return fmt.Errorf("failed to parse book info: %w", err)
	}

	// Replace nil values with "n/a"
	if bookInfo.Title == "" {
		log.Printf("[O'Reilly] ERROR: Invalid book data - no title")
		return fmt.Errorf("invalid book data")
	}

	log.Printf("[O'Reilly] Successfully fetched book info: %s", bookInfo.Title)
	log.Printf("[O'Reilly] Authors: %d, Cover URL: %s", len(bookInfo.Authors), bookInfo.Cover)
	c.bookInfo = &bookInfo
	return nil
}

// GetChapters fetches book chapters (with pagination support)
func (c *Client) GetChapters() error {
	c.updateProgress("chapters", 25, "Retrieving book chapters...")
	log.Printf("[O'Reilly] Fetching chapters for book: %s", c.bookID)

	var allChapters []models.Chapter
	page := 1

	for {
		apiURL := fmt.Sprintf("%s/api/v1/book/%s/chapter/?page=%d", SafariBaseURL, c.bookID, page)
		log.Printf("[O'Reilly] Fetching chapters page %d", page)
		
		resp, err := c.httpClient.Get(apiURL)
		if err != nil {
			log.Printf("[O'Reilly] ERROR: Failed to retrieve chapters: %v", err)
			return fmt.Errorf("failed to retrieve chapters: %w", err)
		}
		defer resp.Body.Close()

		var response struct {
			Results []models.Chapter `json:"results"`
			Next    *string          `json:"next"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
			log.Printf("[O'Reilly] ERROR: Failed to parse chapters: %v", err)
			return fmt.Errorf("failed to parse chapters: %w", err)
		}

		log.Printf("[O'Reilly] Found %d chapters on page %d", len(response.Results), page)

		// Separate cover pages from regular chapters
		var covers []models.Chapter
		var regular []models.Chapter

		for _, ch := range response.Results {
			if strings.Contains(strings.ToLower(ch.Filename), "cover") || 
			   strings.Contains(strings.ToLower(ch.Title), "cover") {
				covers = append(covers, ch)
				log.Printf("[O'Reilly] Found cover chapter: %s", ch.Title)
			} else {
				regular = append(regular, ch)
			}
		}

		// Add covers first, then regular chapters
		allChapters = append(allChapters, covers...)
		allChapters = append(allChapters, regular...)

		// Check if there are more pages
		if response.Next == nil || *response.Next == "" {
			break
		}
		page++
	}

	log.Printf("[O'Reilly] Total chapters found: %d", len(allChapters))
	c.chapters = allChapters
	return nil
}

// createDirectories creates necessary directory structure
func (c *Client) createDirectories() error {
	// Ensure tmp books directory exists
	os.MkdirAll(tmpBooksDir, 0755)
	
	cleanTitle := cleanFilename(c.bookInfo.Title)
	c.bookPath = filepath.Join(tmpBooksDir, fmt.Sprintf("%s (%s)", cleanTitle, c.bookID))

	dirs := []string{
		c.bookPath,
		filepath.Join(c.bookPath, "META-INF"),
		filepath.Join(c.bookPath, "OEBPS"),
		filepath.Join(c.bookPath, "OEBPS", "Images"),
		filepath.Join(c.bookPath, "OEBPS", "Styles"),
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}

	return nil
}

// cleanFilename removes invalid characters from filename
func cleanFilename(name string) string {
	// Remove invalid characters
	reg := regexp.MustCompile(`[^\w\s\-]`)
	clean := reg.ReplaceAllString(name, "")
	
	// Limit length
	if len(clean) > 100 {
		clean = clean[:100]
	}
	
	return strings.TrimSpace(clean)
}

// downloadCover downloads the book cover image
func (c *Client) downloadCover() error {
	if c.bookInfo.Cover == "" {
		log.Printf("[O'Reilly] No cover URL found in book info")
		return nil
	}

	log.Printf("[O'Reilly] Downloading cover from: %s", c.bookInfo.Cover)
	c.updateProgress("cover", 28, "Downloading book cover...")

	resp, err := c.httpClient.Get(c.bookInfo.Cover)
	if err != nil {
		log.Printf("[O'Reilly] ERROR: Failed to download cover: %v", err)
		return fmt.Errorf("failed to download cover: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Printf("[O'Reilly] ERROR: Cover download failed with status: %d", resp.StatusCode)
		return fmt.Errorf("cover download failed: status %d", resp.StatusCode)
	}

	// Determine file extension from Content-Type
	contentType := resp.Header.Get("Content-Type")
	ext := "jpg"
	if strings.Contains(contentType, "png") {
		ext = "png"
	} else if strings.Contains(contentType, "gif") {
		ext = "gif"
	}

	coverFilename := "cover." + ext
	coverPath := filepath.Join(c.bookPath, "OEBPS", "Images", coverFilename)

	// Save cover image
	out, err := os.Create(coverPath)
	if err != nil {
		log.Printf("[O'Reilly] ERROR: Failed to create cover file: %v", err)
		return err
	}
	defer out.Close()

	written, err := io.Copy(out, resp.Body)
	if err != nil {
		log.Printf("[O'Reilly] ERROR: Failed to write cover file: %v", err)
		return err
	}

	log.Printf("[O'Reilly] Cover image downloaded successfully (%d bytes): %s", written, coverFilename)
	c.coverImage = coverFilename
	c.imageFiles = append(c.imageFiles, coverFilename)

	// Create cover.xhtml page
	coverHTML := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8" standalone="no"?>
<!DOCTYPE html>
<html xmlns="http://www.w3.org/1999/xhtml">
<head>
<title>Cover</title>
<style type="text/css">
img { max-width: 100%%; }
</style>
</head>
<body>
<div id="sbo-rt-content">
<img src="Images/%s" alt="Cover"/>
</div>
</body>
</html>`, coverFilename)

	coverHTMLPath := filepath.Join(c.bookPath, "OEBPS", "cover.xhtml")
	if err := os.WriteFile(coverHTMLPath, []byte(coverHTML), 0644); err != nil {
		log.Printf("[O'Reilly] ERROR: Failed to create cover.xhtml: %v", err)
		return err
	}

	log.Printf("[O'Reilly] Created cover.xhtml page")
	return nil
}

// DownloadContent downloads all chapters with concurrency
func (c *Client) DownloadContent() error {
	totalChapters := len(c.chapters)
	log.Printf("[O'Reilly] Starting concurrent download of %d chapters", totalChapters)

	// Use concurrency for faster downloads (max 5 concurrent downloads)
	maxConcurrent := 5
	if totalChapters < maxConcurrent {
		maxConcurrent = totalChapters
	}

	// Create channels for work distribution
	type chapterJob struct {
		chapter *models.Chapter
		idx     int
	}
	
	jobs := make(chan chapterJob, totalChapters)
	results := make(chan error, totalChapters)
	
	// Progress tracking
	completed := 0
	progressChan := make(chan int, totalChapters)

	// Start worker goroutines
	for w := 0; w < maxConcurrent; w++ {
		go func(workerID int) {
			for job := range jobs {
				log.Printf("[O'Reilly] Worker %d: Downloading chapter %d/%d: %s", 
					workerID, job.idx+1, totalChapters, job.chapter.Title)
				
				err := c.downloadChapter(job.chapter, job.idx == 0)
				results <- err
				progressChan <- 1
			}
		}(w)
	}

	// Send jobs to workers
	go func() {
		for idx := range c.chapters {
			jobs <- chapterJob{
				chapter: &c.chapters[idx],
				idx:     idx,
			}
		}
		close(jobs)
	}()

	// Collect results and update progress
	go func() {
		for range progressChan {
			completed++
			progress := 30 + int((float64(completed)/float64(totalChapters))*20)
			c.updateProgress("download", progress,
				fmt.Sprintf("Downloaded %d/%d chapters", completed, totalChapters))
		}
	}()

	// Wait for all downloads to complete
	var lastErr error
	for i := 0; i < totalChapters; i++ {
		if err := <-results; err != nil {
			log.Printf("[O'Reilly] ERROR: Failed to download chapter: %v", err)
			lastErr = err
		}
	}
	
	close(progressChan)

	if lastErr != nil {
		return fmt.Errorf("some chapters failed to download: %w", lastErr)
	}

	log.Printf("[O'Reilly] All %d chapters downloaded successfully", totalChapters)
	return nil
}

// downloadChapter downloads a single chapter
func (c *Client) downloadChapter(chapter *models.Chapter, isFirst bool) error {
	// Fetch HTML content
	resp, err := c.httpClient.Get(chapter.Content)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return err
	}

	// Extract main content
	content := doc.Find("#sbo-rt-content")
	if content.Length() == 0 {
		return fmt.Errorf("book content not found in page")
	}

	// Process stylesheets
	pageCSS := c.processStylesheets(doc, chapter)

	// Convert SVG <image> tags to <img> tags (like Python version)
	c.convertSVGImages(doc)

	// Process images
	c.processImages(content, chapter)

	// Get cover from first page
	if isFirst && c.coverImage == "" {
		c.extractCover(content)
	}

	// Fix links
	c.fixLinks(content)

	// Generate XHTML
	contentHTML, _ := content.Html()
	xhtml := fmt.Sprintf(baseHTML, pageCSS, contentHTML)

	// Save chapter
	filename := strings.Replace(chapter.Filename, ".html", ".xhtml", 1)
	filepath := filepath.Join(c.bookPath, "OEBPS", filename)
	
	return os.WriteFile(filepath, []byte(xhtml), 0644)
}

const baseHTML = `<!DOCTYPE html>
<html lang="en" xmlns="http://www.w3.org/1999/xhtml">
<head>
%s
<style type="text/css">
body{margin:1em;background-color:transparent!important;}
#sbo-rt-content *{text-indent:0pt!important;}
#sbo-rt-content .bq{margin-right:1em!important;}
</style>
</head>
<body>%s</body>
</html>`

// processStylesheets extracts and processes CSS
func (c *Client) processStylesheets(doc *goquery.Document, chapter *models.Chapter) string {
	var pageCSS strings.Builder

	// Add chapter stylesheets
	for _, ss := range chapter.Stylesheets {
		c.mu.Lock()
		if !contains(c.cssFiles, ss.URL) {
			c.cssFiles = append(c.cssFiles, ss.URL)
			cssIdx := len(c.cssFiles) - 1
			c.mu.Unlock()
			c.downloadAsset(ss.URL, "Styles", fmt.Sprintf("Style%02d.css", cssIdx))
			c.mu.Lock()
		}
		idx := indexOf(c.cssFiles, ss.URL)
		c.mu.Unlock()
		pageCSS.WriteString(fmt.Sprintf(`<link href="Styles/Style%02d.css" rel="stylesheet" type="text/css" />`, idx))
		pageCSS.WriteString("\n")
	}

	// Process site styles
	for _, ss := range chapter.SiteStyles {
		c.mu.Lock()
		if !contains(c.cssFiles, ss) {
			c.cssFiles = append(c.cssFiles, ss)
			cssIdx := len(c.cssFiles) - 1
			c.mu.Unlock()
			c.downloadAsset(ss, "Styles", fmt.Sprintf("Style%02d.css", cssIdx))
			c.mu.Lock()
		}
		idx := indexOf(c.cssFiles, ss)
		c.mu.Unlock()
		pageCSS.WriteString(fmt.Sprintf(`<link href="Styles/Style%02d.css" rel="stylesheet" type="text/css" />`, idx))
		pageCSS.WriteString("\n")
	}

	// Process inline <style> tags (like Python version)
	doc.Find("style").Each(func(i int, s *goquery.Selection) {
		// Handle data-template attribute
		if dataTemplate, exists := s.Attr("data-template"); exists && dataTemplate != "" {
			s.SetText(dataTemplate)
			s.RemoveAttr("data-template")
		}
		
		// Get the HTML of the style tag and add it to pageCSS
		styleHTML, err := s.Html()
		if err == nil {
			pageCSS.WriteString("<style>")
			pageCSS.WriteString(styleHTML)
			pageCSS.WriteString("</style>\n")
		}
	})

	return pageCSS.String()
}

// convertSVGImages converts SVG <image> tags to <img> tags for ebook compatibility
func (c *Client) convertSVGImages(doc *goquery.Document) {
	doc.Find("image").Each(func(i int, image *goquery.Selection) {
		// Look for href attribute (could be href, xlink:href, etc.)
		var svgURL string
		for _, attr := range []string{"href", "xlink:href"} {
			if url, exists := image.Attr(attr); exists {
				svgURL = url
				break
			}
		}
		
		if svgURL != "" {
			// Find the parent SVG and its parent
			svg := image.ParentsFiltered("svg").First()
			if svg.Length() > 0 {
				svgParent := svg.Parent()
				
				// Create new img tag
				imgHTML := fmt.Sprintf(`<img src="%s"/>`, svgURL)
				
				// Remove the SVG and add the img tag
				svg.Remove()
				svgParent.AppendHtml(imgHTML)
				
				log.Printf("[O'Reilly] Converted SVG image tag to img: %s", svgURL)
			}
		}
	})
}

// processImages downloads images from chapter metadata and HTML content
func (c *Client) processImages(content *goquery.Selection, chapter *models.Chapter) {
	log.Printf("[O'Reilly] Processing images for chapter: %s", chapter.Title)
	
	assetBaseURL := chapter.AssetBaseURL
	apiV2Detected := strings.Contains(chapter.Content, "/api/v2/")
	
	if apiV2Detected || assetBaseURL == "" {
		assetBaseURL = fmt.Sprintf("%s/api/v2/epubs/urn:orm:book:%s/files", SafariBaseURL, c.bookID)
		log.Printf("[O'Reilly] Using API v2 asset base URL")
	}

	// Download images from chapter metadata
	log.Printf("[O'Reilly] Chapter has %d images in metadata", len(chapter.Images))
	for _, imgURL := range chapter.Images {
		fullURL := imgURL
		if !strings.HasPrefix(imgURL, "http") {
			if apiV2Detected {
				fullURL = assetBaseURL + "/" + imgURL
			} else {
				fullURL = assetBaseURL + "/" + imgURL
			}
		}
		
		filename := filepath.Base(imgURL)
		c.mu.Lock()
		alreadyExists := contains(c.imageFiles, filename)
		if !alreadyExists {
			c.imageFiles = append(c.imageFiles, filename)
		}
		c.mu.Unlock()
		
		if !alreadyExists {
			log.Printf("[O'Reilly] Downloading image from metadata: %s", filename)
			if err := c.downloadAsset(fullURL, "Images", filename); err != nil {
				log.Printf("[O'Reilly] WARNING: Failed to download image %s: %v", filename, err)
			}
		}
	}
	
	// Also scan HTML content for images and download them
	content.Find("img").Each(func(i int, img *goquery.Selection) {
		src, exists := img.Attr("src")
		if exists && src != "" {
			// Determine full URL for the image
			var fullURL string
			filename := filepath.Base(src)
			
			if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
				fullURL = src
			} else if strings.HasPrefix(src, "/") {
				// Absolute path
				fullURL = SafariBaseURL + src
			} else if strings.Contains(src, "images/") || strings.Contains(src, "graphics/") || strings.Contains(src, "cover") {
				// Relative image path
				if apiV2Detected {
					fullURL = assetBaseURL + "/" + src
				} else {
					fullURL = assetBaseURL + "/" + src
				}
			} else {
				// Try asset base URL
				fullURL = assetBaseURL + "/" + src
			}
			
			c.mu.Lock()
			alreadyExists := contains(c.imageFiles, filename)
			if !alreadyExists {
				c.imageFiles = append(c.imageFiles, filename)
			}
			c.mu.Unlock()
			
			if !alreadyExists {
				log.Printf("[O'Reilly] Downloading image from HTML: %s (from src: %s)", filename, src)
				if err := c.downloadAsset(fullURL, "Images", filename); err != nil {
					log.Printf("[O'Reilly] WARNING: Failed to download image %s from %s: %v", filename, fullURL, err)
				}
			}
		}
	})
	
	log.Printf("[O'Reilly] Total unique images collected: %d", len(c.imageFiles))
}

// downloadAsset downloads an asset (CSS or image)
func (c *Client) downloadAsset(url, subdir, filename string) error {
	log.Printf("[O'Reilly] Downloading asset: %s to %s/%s", url, subdir, filename)
	
	resp, err := c.httpClient.Get(url)
	if err != nil {
		log.Printf("[O'Reilly] ERROR: Failed to download asset from %s: %v", url, err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Printf("[O'Reilly] ERROR: Asset download failed with status %d: %s", resp.StatusCode, url)
		return fmt.Errorf("download failed with status %d", resp.StatusCode)
	}

	assetPath := filepath.Join(c.bookPath, "OEBPS", subdir, filename)
	file, err := os.Create(assetPath)
	if err != nil {
		log.Printf("[O'Reilly] ERROR: Failed to create file %s: %v", assetPath, err)
		return err
	}
	defer file.Close()

	written, err := io.Copy(file, resp.Body)
	if err != nil {
		log.Printf("[O'Reilly] ERROR: Failed to write asset %s: %v", filename, err)
		return err
	}
	
	log.Printf("[O'Reilly] Successfully downloaded asset: %s (%d bytes)", filename, written)
	return nil
}

// extractCover extracts cover image
func (c *Client) extractCover(content *goquery.Selection) {
	content.Find("img").Each(func(i int, img *goquery.Selection) {
		src, exists := img.Attr("src")
		if exists && (strings.Contains(src, "cover") || i == 0) {
			c.coverImage = filepath.Base(src)
		}
	})
}

// fixLinks fixes relative links in content (matching Python link_replace logic)
func (c *Client) fixLinks(content *goquery.Selection) {
	content.Find("a").Each(func(i int, a *goquery.Selection) {
		href, exists := a.Attr("href")
		if !exists || strings.HasPrefix(href, "mailto") {
			return
		}
		
		// Handle absolute URLs
		if strings.HasPrefix(href, "http") {
			// If URL contains book ID, make it relative
			if strings.Contains(href, c.bookID) {
				parts := strings.Split(href, c.bookID)
				if len(parts) > 1 {
					href = parts[1]
				}
			} else {
				return // Keep external URLs as-is
			}
		}
		
		// Replace .html with .xhtml
		newHref := strings.Replace(href, ".html", ".xhtml", 1)
		a.SetAttr("href", newHref)
	})

	content.Find("img").Each(func(i int, img *goquery.Selection) {
		src, exists := img.Attr("src")
		if !exists {
			return
		}
		
		// Check if this is an image path (not absolute URL)
		if !strings.HasPrefix(src, "http") {
			// Check if it's already an image path or needs to be converted
			if strings.Contains(src, "cover") || 
			   strings.Contains(src, "images") || 
			   strings.Contains(src, "graphics") ||
			   strings.HasSuffix(src, ".png") ||
			   strings.HasSuffix(src, ".jpg") ||
			   strings.HasSuffix(src, ".jpeg") ||
			   strings.HasSuffix(src, ".gif") {
				img.SetAttr("src", "Images/"+filepath.Base(src))
			}
		} else {
			// For absolute URLs, check if they contain book ID
			if strings.Contains(src, c.bookID) {
				img.SetAttr("src", "Images/"+filepath.Base(src))
			}
		}
	})
}

// Helper functions
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func indexOf(slice []string, item string) int {
	for i, s := range slice {
		if s == item {
			return i
		}
	}
	return -1
}

// GetBookTitle returns the book title
func (c *Client) GetBookTitle() string {
	if c.bookInfo != nil {
		return c.bookInfo.Title
	}
	return "Unknown"
}

// GetBookInfoData returns the book info
func (c *Client) GetBookInfoData() *models.BookInfo {
	return c.bookInfo
}

// CreateEPUB generates the EPUB file
func (c *Client) CreateEPUB() (string, error) {
	c.updateProgress("epub", 50, "Creating EPUB structure...")

	// Create META-INF/container.xml
	containerXML := `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
<rootfiles>
<rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml" />
</rootfiles>
</container>`
	
	if err := os.WriteFile(filepath.Join(c.bookPath, "META-INF", "container.xml"), []byte(containerXML), 0644); err != nil {
		return "", err
	}

	// Create mimetype
	if err := os.WriteFile(filepath.Join(c.bookPath, "mimetype"), []byte("application/epub+zip"), 0644); err != nil {
		return "", err
	}

	// Create content.opf
	c.updateProgress("epub", 60, "Generating content.opf...")
	contentOPF, err := c.createContentOPF()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(c.bookPath, "OEBPS", "content.opf"), []byte(contentOPF), 0644); err != nil {
		return "", err
	}

	// Create toc.ncx
	c.updateProgress("epub", 70, "Generating toc.ncx...")
	tocNCX, err := c.createTOC()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(c.bookPath, "OEBPS", "toc.ncx"), []byte(tocNCX), 0644); err != nil {
		return "", err
	}

	// Create ZIP/EPUB
	c.updateProgress("epub", 80, "Packaging EPUB...")
	epubPath := filepath.Join(c.bookPath, c.bookID+".epub")
	if err := c.createZIP(epubPath); err != nil {
		return "", err
	}

	c.updateProgress("epub", 100, "EPUB created successfully!")
	return epubPath, nil
}

// createContentOPF generates content.opf file
func (c *Client) createContentOPF() (string, error) {
	log.Printf("[O'Reilly] Creating content.opf manifest...")
	
	var manifest strings.Builder
	var spine strings.Builder

	// Add cover.xhtml first if we have a cover
	if c.coverImage != "" {
		log.Printf("[O'Reilly] Adding cover.xhtml to manifest and spine")
		manifest.WriteString(`<item id="cover" href="cover.xhtml" media-type="application/xhtml+xml" />`)
		manifest.WriteString("\n")
		spine.WriteString(`<itemref idref="cover"/>`)
		spine.WriteString("\n")
	}

	// Add chapters
	log.Printf("[O'Reilly] Adding %d chapters to manifest", len(c.chapters))
	for _, chapter := range c.chapters {
		filename := strings.Replace(chapter.Filename, ".html", ".xhtml", 1)
		itemID := html.EscapeString(strings.TrimSuffix(filename, filepath.Ext(filename)))
		
		manifest.WriteString(fmt.Sprintf(`<item id="%s" href="%s" media-type="application/xhtml+xml" />`, itemID, filename))
		manifest.WriteString("\n")
		
		spine.WriteString(fmt.Sprintf(`<itemref idref="%s"/>`, itemID))
		spine.WriteString("\n")
	}

	// Add images
	log.Printf("[O'Reilly] Adding %d images to manifest", len(c.imageFiles))
	for _, img := range c.imageFiles {
		ext := strings.ToLower(filepath.Ext(img))
		imgName := strings.TrimSuffix(img, ext)
		
		// Determine correct MIME type
		mimeType := "image/jpeg"
		if ext == ".png" {
			mimeType = "image/png"
		} else if ext == ".gif" {
			mimeType = "image/gif"
		} else if ext == ".svg" {
			mimeType = "image/svg+xml"
		}
		
		// Use "coverimg" as ID for cover image
		imgID := "img_" + html.EscapeString(imgName)
		if img == c.coverImage {
			imgID = "coverimg"
		}
		
		manifest.WriteString(fmt.Sprintf(`<item id="%s" href="Images/%s" media-type="%s" />`,
			imgID, img, mimeType))
		manifest.WriteString("\n")
	}

	// Add CSS
	log.Printf("[O'Reilly] Adding %d CSS files to manifest", len(c.cssFiles))
	for i := range c.cssFiles {
		manifest.WriteString(fmt.Sprintf(`<item id="style_%02d" href="Styles/Style%02d.css" media-type="text/css" />`, i, i))
		manifest.WriteString("\n")
	}

	// Build authors
	var authors strings.Builder
	for _, author := range c.bookInfo.Authors {
		authors.WriteString(fmt.Sprintf(`<dc:creator opf:file-as="%s" opf:role="aut">%s</dc:creator>`,
			html.EscapeString(author.Name), html.EscapeString(author.Name)))
		authors.WriteString("\n")
	}

	// Build subjects
	var subjects strings.Builder
	for _, subject := range c.bookInfo.Subjects {
		subjects.WriteString(fmt.Sprintf(`<dc:subject>%s</dc:subject>`, html.EscapeString(subject.Name)))
		subjects.WriteString("\n")
	}

	// Build publishers
	var publishers strings.Builder
	for _, pub := range c.bookInfo.Publishers {
		if publishers.Len() > 0 {
			publishers.WriteString(", ")
		}
		publishers.WriteString(html.EscapeString(pub.Name))
	}

	isbn := c.bookInfo.ISBN
	if isbn == "" {
		isbn = c.bookID
	}

	// Cover reference for guide
	coverPageRef := "cover.xhtml"
	if c.coverImage == "" && len(c.chapters) > 0 {
		coverPageRef = strings.Replace(c.chapters[0].Filename, ".html", ".xhtml", 1)
	}

	contentOPF := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<package xmlns="http://www.idpf.org/2007/opf" unique-identifier="bookid" version="2.0">
<metadata xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:opf="http://www.idpf.org/2007/opf">
<dc:title>%s</dc:title>
%s
<dc:description>%s</dc:description>
%s
<dc:publisher>%s</dc:publisher>
<dc:rights>%s</dc:rights>
<dc:language>en-US</dc:language>
<dc:date>%s</dc:date>
<dc:identifier id="bookid">%s</dc:identifier>
<meta name="cover" content="coverimg"/>
</metadata>
<manifest>
<item id="ncx" href="toc.ncx" media-type="application/x-dtbncx+xml" />
%s
</manifest>
<spine toc="ncx">
%s
</spine>
<guide><reference href="%s" title="Cover" type="cover" /></guide>
</package>`,
		html.EscapeString(c.bookInfo.Title),
		authors.String(),
		html.EscapeString(c.bookInfo.Description),
		subjects.String(),
		publishers.String(),
		html.EscapeString(c.bookInfo.Rights),
		c.bookInfo.Issued,
		isbn,
		manifest.String(),
		spine.String(),
		coverPageRef,
	)

	log.Printf("[O'Reilly] content.opf created successfully")
	return contentOPF, nil
}

// createTOC generates toc.ncx file
func (c *Client) createTOC() (string, error) {
	apiURL := fmt.Sprintf("%s/api/v1/book/%s/toc/", SafariBaseURL, c.bookID)
	resp, err := c.httpClient.Get(apiURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var toc []models.TOCItem
	if err := json.NewDecoder(resp.Body).Decode(&toc); err != nil {
		return "", err
	}

	navMap, maxDepth := c.parseTOC(toc, 1)

	authors := ""
	if len(c.bookInfo.Authors) > 0 {
		authors = c.bookInfo.Authors[0].Name
	}

	isbn := c.bookInfo.ISBN
	if isbn == "" {
		isbn = c.bookID
	}

	tocNCX := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8" standalone="no" ?>
<!DOCTYPE ncx PUBLIC "-//NISO//DTD ncx 2005-1//EN" "http://www.daisy.org/z3986/2005/ncx-2005-1.dtd">
<ncx xmlns="http://www.daisy.org/z3986/2005/ncx/" version="2005-1">
<head>
<meta content="ID:ISBN:%s" name="dtb:uid"/>
<meta content="%d" name="dtb:depth"/>
<meta content="0" name="dtb:totalPageCount"/>
<meta content="0" name="dtb:maxPageNumber"/>
</head>
<docTitle><text>%s</text></docTitle>
<docAuthor><text>%s</text></docAuthor>
<navMap>%s</navMap>
</ncx>`,
		isbn,
		maxDepth,
		html.EscapeString(c.bookInfo.Title),
		html.EscapeString(authors),
		navMap,
	)

	return tocNCX, nil
}

// parseTOC recursively parses TOC items
func (c *Client) parseTOC(items []models.TOCItem, playOrder int) (string, int) {
	var result strings.Builder
	maxDepth := 0

	for _, item := range items {
		depth := item.Depth
		if depth == 0 {
			depth = 1
		}
		if depth > maxDepth {
			maxDepth = depth
		}

		id := item.Fragment
		if id == "" {
			id = item.ID
		}

		href := strings.Replace(filepath.Base(item.Href), ".html", ".xhtml", 1)

		result.WriteString(fmt.Sprintf(`<navPoint id="%s" playOrder="%d">`,
			html.EscapeString(id), playOrder))
		result.WriteString(fmt.Sprintf(`<navLabel><text>%s</text></navLabel>`,
			html.EscapeString(item.Label)))
		result.WriteString(fmt.Sprintf(`<content src="%s"/>`, href))

		if len(item.Children) > 0 {
			childNav, childDepth := c.parseTOC(item.Children, playOrder+1)
			result.WriteString(childNav)
			if childDepth > maxDepth {
				maxDepth = childDepth
			}
			playOrder += len(item.Children)
		}

		result.WriteString("</navPoint>\n")
		playOrder++
	}

	return result.String(), maxDepth
}

// createZIP creates the EPUB ZIP file
func (c *Client) createZIP(epubPath string) error {
	file, err := os.Create(epubPath)
	if err != nil {
		return err
	}
	defer file.Close()

	w := zip.NewWriter(file)
	defer w.Close()

	// Add mimetype first (uncompressed)
	mimeWriter, err := w.CreateHeader(&zip.FileHeader{
		Name:   "mimetype",
		Method: zip.Store,
	})
	if err != nil {
		return err
	}
	mimeWriter.Write([]byte("application/epub+zip"))

	// Add all other files
	return filepath.Walk(c.bookPath, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || strings.HasSuffix(path, ".epub") {
			return err
		}

		relPath, err := filepath.Rel(c.bookPath, path)
		if err != nil {
			return err
		}

		zipFile, err := w.Create(relPath)
		if err != nil {
			return err
		}

		fsFile, err := os.Open(path)
		if err != nil {
			return err
		}
		defer fsFile.Close()

		_, err = io.Copy(zipFile, fsFile)
		return err
	})
}

// Download is the main download function
func (c *Client) Download() (string, error) {
	log.Printf("[O'Reilly] ===== Starting book download =====")
	
	// Get book info
	log.Printf("[O'Reilly] Step 1: Fetching book info...")
	if err := c.GetBookInfo(); err != nil {
		return "", err
	}

	// Get chapters
	log.Printf("[O'Reilly] Step 2: Fetching chapters...")
	if err := c.GetChapters(); err != nil {
		return "", err
	}

	// Create directories
	log.Printf("[O'Reilly] Step 3: Creating directory structure...")
	if err := c.createDirectories(); err != nil {
		return "", err
	}

	// Download cover
	log.Printf("[O'Reilly] Step 4: Downloading cover image...")
	if err := c.downloadCover(); err != nil {
		log.Printf("[O'Reilly] WARNING: Cover download failed: %v", err)
		// Continue even if cover fails
	}

	// Download content
	log.Printf("[O'Reilly] Step 5: Downloading chapter content...")
	if err := c.DownloadContent(); err != nil {
		return "", err
	}

	// Create EPUB
	log.Printf("[O'Reilly] Step 6: Creating EPUB file...")
	epubPath, err := c.CreateEPUB()
	if err != nil {
		log.Printf("[O'Reilly] ERROR: EPUB creation failed: %v", err)
		return "", err
	}
	
	log.Printf("[O'Reilly] ===== Download completed successfully =====")
	log.Printf("[O'Reilly] EPUB created at: %s", epubPath)
	return epubPath, nil
}
