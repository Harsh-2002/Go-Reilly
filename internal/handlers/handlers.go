package handlers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"goreilly/internal/cache"
	"goreilly/internal/models"
	"goreilly/internal/oreilly"
	"goreilly/internal/storage"
)

var (
	downloads     = make(map[string]*models.Download)
	downloadsLock sync.RWMutex
	
	// Redis and MinIO clients
	RedisClient *cache.RedisClient
	MinIOClient *storage.MinIOClient
)

const (
	convertedDir = "Converted"
	cookiesPath  = "cookies.json"
)

func init() {
	// Ensure Converted directory exists
	os.MkdirAll(convertedDir, 0755)
}

// DownloadBookHandler handles book download requests
func DownloadBookHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("[Handler] Download request received")
	
	var req struct {
		BookID string `json:"book_id"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("[Handler] ERROR: Failed to decode request: %v", err)
		http.Error(w, `{"error":"Invalid request"}`, http.StatusBadRequest)
		return
	}

	bookID := req.BookID
	if bookID == "" {
		log.Printf("[Handler] ERROR: Empty book ID")
		http.Error(w, `{"error":"Book ID is required"}`, http.StatusBadRequest)
		return
	}
	
	log.Printf("[Handler] Processing book ID: %s", bookID)

	// Check if book is cached in Redis
	if RedisClient != nil && MinIOClient != nil {
		cachedInfo, err := RedisClient.GetBookInfo(bookID)
		if err == nil && cachedInfo != nil {
			// Verify file exists in MinIO
			exists, objectName, fileSize, err := MinIOClient.FileExists(bookID)
			if err == nil && exists {
				// Generate presigned URL (valid for 1 hour)
				presignedURL, err := MinIOClient.GetPresignedURL(objectName, time.Hour*1)
				if err == nil {
					log.Printf("[Download] Cached: %s", bookID)
					// Create download ID for tracking
					downloadID := uuid.New().String()
					
					// Store in downloads map
					download := &models.Download{
						ID:        downloadID,
						BookID:    bookID,
						Status:    "completed",
						Progress:  100,
						Message:   "Book retrieved from cache",
						BookTitle: cachedInfo.BookTitle,
						FileSize:  fileSize,
						FilePath:  objectName,
						Timestamp: time.Now().Unix(),
						Cached:    true,
						MinIOURL:  presignedURL,
					}
					
					downloadsLock.Lock()
					downloads[downloadID] = download
					downloadsLock.Unlock()
					
					// Return cached response
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					json.NewEncoder(w).Encode(map[string]interface{}{
						"download_id": downloadID,
						"cached":      true,
						"book_title":  cachedInfo.BookTitle,
						"file_size":   fileSize,
						"minio_url":   presignedURL,
						"uploaded_at": cachedInfo.UploadedAt,
					})
					return
				}
			}
		}
	}

	// Book not in cache, proceed with normal download
	downloadID := uuid.New().String()
	log.Printf("[Download] Starting: %s", bookID)

	// Initialize download
	download := &models.Download{
		ID:        downloadID,
		BookID:    bookID,
		Status:    "starting",
		Progress:  0,
		Message:   "Initializing download...",
		Timestamp: time.Now().Unix(),
	}

	downloadsLock.Lock()
	downloads[downloadID] = download
	downloadsLock.Unlock()

	// Start download in goroutine
	go downloadBookAsync(downloadID, bookID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{
		"download_id": downloadID,
		"cached":      "false",
	})
}

// downloadBookAsync downloads book asynchronously
func downloadBookAsync(downloadID, bookID string) {
	downloadsLock.RLock()
	download := downloads[downloadID]
	downloadsLock.RUnlock()

	if download == nil {
		return
	}

	// Progress callback
	progressCallback := func(stage string, progress int, message string) {
		download.UpdateStatus("downloading", message, progress)
	}

	// Create client
	download.UpdateStatus("downloading", "Connecting to O'Reilly...", 10)
	
	client, err := oreilly.NewClient(bookID, cookiesPath, progressCallback)
	if err != nil {
		download.SetError(formatError(err))
		return
	}

	// Download book
	download.UpdateStatus("downloading", "Downloading book content...", 20)
	epubPath, err := client.Download()
	if err != nil {
		download.SetError(formatError(err))
		return
	}

	// Convert with Calibre
	download.UpdateStatus("downloading", "Converting with Calibre...", 80)
	
	bookTitle := client.GetBookTitle()
	safeFilename := cleanFilename(bookTitle)
	outputFile := filepath.Join(convertedDir, fmt.Sprintf("%s_%s.epub", safeFilename, bookID))

	// Try Calibre conversion
	if err := convertWithCalibre(epubPath, outputFile); err != nil {
		// Fallback: just copy the file
		if err := copyFile(epubPath, outputFile); err != nil {
			download.SetError(fmt.Sprintf("Failed to save file: %v", err))
			return
		}
	}

	// Get file size
	fileInfo, err := os.Stat(outputFile)
	var fileSize int64
	if err == nil {
		fileSize = fileInfo.Size()
	}

	// Upload to MinIO and cache in Redis
	var minioURL string
	if MinIOClient != nil {
		download.UpdateStatus("downloading", "Uploading to storage...", 90)
		log.Printf("[Upload] Starting upload for book %s", bookID)
		
		objectName, uploadedSize, err := MinIOClient.UploadFile(bookID, outputFile)
		if err != nil {
			log.Printf("[Upload] ERROR: Failed to upload to MinIO: %v", err)
		} else {
			log.Printf("[Upload] Success: %s", objectName)
			
			// Generate presigned URL (valid for 1 hour)
			presignedURL, err := MinIOClient.GetPresignedURL(objectName, time.Hour*1)
			if err != nil {
				log.Printf("[Upload] ERROR: Failed to generate URL: %v", err)
			} else {
				minioURL = presignedURL
				
				// Cache in Redis if available
				if RedisClient != nil {
					cacheInfo := &cache.BookCacheInfo{
						BookID:     bookID,
						BookTitle:  bookTitle,
						MinIOPath:  objectName,
						FileSize:   uploadedSize,
						UploadedAt: time.Now(),
					}
					
					if err := RedisClient.SetBookInfo(cacheInfo); err != nil {
						log.Printf("[Cache] ERROR: Failed to cache: %v", err)
					}
				}
			}
		}
	}

	// Update status to completed
	downloadsLock.Lock()
	download.Status = "completed"
	download.Progress = 100
	download.Message = "Download complete!"
	download.FilePath = outputFile
	download.BookTitle = bookTitle
	download.FileSize = fileSize
	download.MinIOURL = minioURL
	download.Timestamp = time.Now().Unix()
	downloadsLock.Unlock()
}

// convertWithCalibre converts EPUB using Calibre
func convertWithCalibre(inputPath, outputPath string) error {
	cmd := exec.Command("ebook-convert", inputPath, outputPath)
	cmd.Stdout = nil
	cmd.Stderr = nil
	
	// Set timeout
	timer := time.AfterFunc(5*time.Minute, func() {
		cmd.Process.Kill()
	})
	defer timer.Stop()

	return cmd.Run()
}

// copyFile copies a file
func copyFile(src, dst string) error {
	input, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, input, 0644)
}

// cleanFilename removes invalid characters
func cleanFilename(name string) string {
	result := ""
	for _, c := range name {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == ' ' || c == '-' || c == '_' {
			result += string(c)
		}
	}
	if len(result) > 100 {
		result = result[:100]
	}
	return result
}

// formatError formats error messages for users
func formatError(err error) string {
	msg := err.Error()
	
	if contains(msg, "Book not found") || contains(msg, "API error") {
		return "Book not found. Please check the Book ID and try again."
	}
	if contains(msg, "Authentication failed") || contains(msg, "cookies.json") {
		return "Authentication failed. Please update your cookies.json file."
	}
	if contains(msg, "timeout") || contains(msg, "Timeout") {
		return "Request timed out. Please try again."
	}
	
	return msg
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && 
		(s[:len(substr)] == substr || s[len(s)-len(substr):] == substr || 
		findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// GetStatusHandler returns download status
func GetStatusHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	downloadID := vars["id"]

	downloadsLock.RLock()
	download, exists := downloads[downloadID]
	downloadsLock.RUnlock()

	if !exists {
		http.Error(w, `{"error":"Download ID not found"}`, http.StatusNotFound)
		return
	}

	// Create response (without internal fields)
	response := map[string]interface{}{
		"status":     download.Status,
		"progress":   download.Progress,
		"message":    download.Message,
		"book_id":    download.BookID,
		"book_title": download.BookTitle,
		"file_size":  download.FileSize,
		"cached":     download.Cached,
	}

	if download.Error != "" {
		response["error"] = download.Error
	}
	
	if download.MinIOURL != "" {
		response["minio_url"] = download.MinIOURL
	}
	
	if !download.UploadedAt.IsZero() {
		response["uploaded_at"] = download.UploadedAt
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// GetFileHandler serves the downloaded file
func GetFileHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	downloadID := vars["id"]

	downloadsLock.RLock()
	download, exists := downloads[downloadID]
	downloadsLock.RUnlock()

	if !exists {
		http.Error(w, `{"error":"Download ID not found"}`, http.StatusNotFound)
		return
	}

	if download.Status != "completed" {
		http.Error(w, `{"error":"Download not completed"}`, http.StatusBadRequest)
		return
	}

	// If we have a MinIO URL, redirect to it
	if download.MinIOURL != "" {
		http.Redirect(w, r, download.MinIOURL, http.StatusTemporaryRedirect)
		return
	}

	// Fallback to local file
	if download.FilePath == "" || !fileExists(download.FilePath) {
		http.Error(w, `{"error":"File not found"}`, http.StatusNotFound)
		return
	}

	// Serve file
	w.Header().Set("Content-Type", "application/epub+zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.epub"`, cleanFilename(download.BookTitle)))
	http.ServeFile(w, r, download.FilePath)
}

// GetFileInfoHandler returns file information
func GetFileInfoHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	downloadID := vars["id"]

	downloadsLock.RLock()
	download, exists := downloads[downloadID]
	downloadsLock.RUnlock()

	if !exists {
		http.Error(w, `{"error":"Download ID not found"}`, http.StatusNotFound)
		return
	}

	if download.Status != "completed" {
		http.Error(w, `{"error":"Download not completed"}`, http.StatusBadRequest)
		return
	}

	response := map[string]interface{}{
		"title":       download.BookTitle,
		"format":      "EPUB",
		"size":        download.FileSize,
		"download_id": downloadID,
		"book_id":     download.BookID,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// fileExists checks if file exists
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// GetBookInfoHandler fetches book metadata without downloading
func GetBookInfoHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	bookID := vars["id"]

	// Create a temporary client just to fetch book info
	client, err := oreilly.NewClient(bookID, cookiesPath, nil)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"Failed to connect: %s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	// Fetch book info
	if err := client.GetBookInfo(); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"Failed to fetch book info: %s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	bookInfo := client.GetBookInfoData()
	
	// Build authors string
	authors := []string{}
	for _, author := range bookInfo.Authors {
		authors = append(authors, author.Name)
	}

	// Build publishers string
	publishers := []string{}
	for _, pub := range bookInfo.Publishers {
		publishers = append(publishers, pub.Name)
	}

	response := map[string]interface{}{
		"id":          bookInfo.ID,
		"title":       bookInfo.Title,
		"authors":     authors,
		"description": bookInfo.Description,
		"cover":       bookInfo.Cover,
		"publishers":  publishers,
		"issued":      bookInfo.Issued,
		"isbn":        bookInfo.ISBN,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}
