package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	
	// Semaphore to limit concurrent downloads (max 3 simultaneous)
	downloadSemaphore = make(chan struct{}, 3)
	
	// Worker pool for conversions (max 2 simultaneous conversions)
	conversionSemaphore = make(chan struct{}, 2)
	
	// Redis and MinIO clients
	RedisClient *cache.RedisClient
	MinIOClient *storage.MinIOClient
	
	// Presigned URL expiry duration (configured at startup)
	PresignedURLExpiry time.Duration
)

const (
	tmpDir      = "/tmp/goreilly"
	cookiesPath = "cookies.json"
)

func init() {
	// Ensure tmp directory exists with proper permissions
	os.MkdirAll(tmpDir, 0755)
	
	// Clean any leftover files from previous runs
	log.Printf("[Init] Cleaning tmp directory: %s", tmpDir)
	if err := os.RemoveAll(tmpDir); err != nil {
		log.Printf("[Init] WARNING: Failed to clean tmp directory: %v", err)
	}
	os.MkdirAll(tmpDir, 0755)
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
			log.Printf("[Cache] Found cached book: %s", bookID)
			
			// Generate fresh presigned URL on-demand (not stored in cache)
			var presignedEpubURL string
			var epubSize int64
			
			// Generate EPUB URL if path exists
			if cachedInfo.EpubPath != "" {
				if url, err := MinIOClient.GetPresignedURL(cachedInfo.EpubPath, PresignedURLExpiry); err == nil {
					presignedEpubURL = url
					epubSize = cachedInfo.EpubSize
					log.Printf("[Cache] Generated fresh EPUB URL (expires in %d hours)", int(PresignedURLExpiry.Hours()))
				}
			}
			
			// If EPUB exists, return cached response
			if presignedEpubURL != "" {
				log.Printf("[Download] Cached: %s (EPUB)", bookID)
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
					FileSize:  epubSize,
					EpubSize:  epubSize,
					FilePath:  "", // No local file - using MinIO only
					Timestamp: time.Now().Unix(),
					Cached:    true,
					MinIOURL:  presignedEpubURL,
					EpubURL:   presignedEpubURL,
				}
				
				downloadsLock.Lock()
				downloads[downloadID] = download
				downloadsLock.Unlock()
				
				// Cleanup cached download from memory after 5 minutes
				go func() {
					time.Sleep(5 * time.Minute)
					downloadsLock.Lock()
					if _, exists := downloads[downloadID]; exists {
						log.Printf("[Cleanup] Removing cached download from memory: %s", downloadID)
						delete(downloads, downloadID)
					}
					downloadsLock.Unlock()
				}()
				
				// Return cached response
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"download_id": downloadID,
					"cached":      true,
					"book_title":  cachedInfo.BookTitle,
					"file_size":   epubSize,
					"epub_size":   epubSize,
					"epub_url":    presignedEpubURL,
					"minio_url":   presignedEpubURL, // Backwards compatibility
					"uploaded_at": cachedInfo.UploadedAt,
				})
				return
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
	// Cleanup helper function
	cleanupDownload := func(id string) {
		downloadsLock.Lock()
		defer downloadsLock.Unlock()
		if download, exists := downloads[id]; exists {
			log.Printf("[Cleanup] Removing download from memory: %s (Status: %s)", id, download.Status)
			delete(downloads, id)
		}
	}
	
	// Acquire semaphore slot (limit concurrent downloads)
	select {
	case downloadSemaphore <- struct{}{}:
		// Got slot, proceed
		defer func() { <-downloadSemaphore }() // Release slot when done
	default:
		// No slots available, queue the request
		log.Printf("[Queue] Download %s waiting for available slot...", downloadID)
		downloadSemaphore <- struct{}{} // Block until slot available
		defer func() { <-downloadSemaphore }()
		log.Printf("[Queue] Download %s acquired slot", downloadID)
	}
	
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
		download.SetError(formatError(err), cleanupDownload)
		return
	}

	// Download book
	download.UpdateStatus("downloading", "Downloading book content...", 20)
	epubPath, err := client.Download()
	if err != nil {
		download.SetError(formatError(err), cleanupDownload)
		return
	}
	
	// Defer cleanup of original downloaded book (from Books directory)
	defer func() {
		if epubPath != "" {
			log.Printf("[Cleanup] Removing original download: %s", epubPath)
			// Also try to remove the parent directory (book folder in Books/)
			bookDir := filepath.Dir(epubPath)
			if err := os.RemoveAll(bookDir); err != nil {
				log.Printf("[Cleanup] WARNING: Failed to remove book directory: %v", err)
			} else {
				log.Printf("[Cleanup] Book directory removed: %s", bookDir)
			}
		}
	}()

	// Convert with Calibre (with concurrency control)
	download.UpdateStatus("downloading", "Converting with Calibre...", 80)
	
	bookTitle := client.GetBookTitle()
	safeFilename := cleanFilename(bookTitle)
	
	// Use /tmp for temporary conversion file
	outputEpubFile := filepath.Join(tmpDir, fmt.Sprintf("%s_%s.epub", safeFilename, bookID))

	// Acquire conversion semaphore (CPU-intensive operations)
	log.Printf("[Conversion] Waiting for conversion slot...")
	conversionSemaphore <- struct{}{}
	log.Printf("[Conversion] Acquired conversion slot")
	
	// Convert to EPUB
	epubErr := convertWithCalibre(epubPath, outputEpubFile)
	if epubErr != nil {
		// Fallback: just copy the file
		if err := copyFile(epubPath, outputEpubFile); err != nil {
			<-conversionSemaphore // Release semaphore before returning
			download.SetError(fmt.Sprintf("Failed to save EPUB file: %v", err), cleanupDownload)
			return
		}
	}
	
	// Release conversion semaphore
	<-conversionSemaphore
	log.Printf("[Conversion] Released conversion slot")

	// Get file size
	epubFileInfo, err := os.Stat(outputEpubFile)
	var epubFileSize int64
	if err == nil {
		epubFileSize = epubFileInfo.Size()
	}

	// Upload EPUB to MinIO
	var minioEpubURL string
	var uploadedEpubSize int64
	var epubObjectName string
	
	if MinIOClient != nil {
		download.UpdateStatus("downloading", "Uploading to storage...", 90)
		log.Printf("[Upload] Starting upload for book %s", bookID)
		
		// Upload EPUB
		epubObj, epubSize, err := MinIOClient.UploadFile(bookID, outputEpubFile)
		if err != nil {
			log.Printf("[Upload] ERROR: Failed to upload EPUB to MinIO: %v", err)
			download.SetError("Failed to upload to storage", cleanupDownload)
			return
		}
		
		epubObjectName = epubObj
		uploadedEpubSize = epubSize
		
		log.Printf("[Upload] EPUB Success: %s", epubObjectName)
		
		// Generate presigned URL for EPUB (valid for configured duration)
		presignedEpubURL, err := MinIOClient.GetPresignedURL(epubObjectName, PresignedURLExpiry)
		if err != nil {
			log.Printf("[Upload] ERROR: Failed to generate EPUB URL: %v", err)
			download.SetError("Failed to generate download URL", cleanupDownload)
			return
		}
		
		minioEpubURL = presignedEpubURL
		
	// Delete local EPUB file after successful upload
	log.Printf("[Cleanup] Removing local EPUB file: %s", outputEpubFile)
	if err := os.Remove(outputEpubFile); err != nil {
		log.Printf("[Cleanup] WARNING: Failed to remove local EPUB: %v", err)
	} else {
		log.Printf("[Cleanup] Local EPUB removed successfully")
	}
	
	log.Printf("[Upload] Upload completed for book %s", bookID)		// Cache book metadata in Redis (store path, not URL)
		if RedisClient != nil && epubObjectName != "" {
			cacheInfo := &cache.BookCacheInfo{
				BookID:     bookID,
				BookTitle:  bookTitle,
				EpubPath:   epubObjectName,
				EpubSize:   uploadedEpubSize,
				UploadedAt: time.Now(),
			}
			
			if err := RedisClient.SetBookInfo(cacheInfo); err != nil {
				log.Printf("[Cache] ERROR: Failed to cache book metadata: %v", err)
			} else {
				log.Printf("[Cache] Stored book metadata (path only, URL generated on-demand)")
			}
		}
	} else {
		// MinIO is disabled - cannot proceed without storage
		log.Printf("[Upload] ERROR: MinIO is disabled - cannot complete download")
		download.SetError("Storage service unavailable - please contact administrator", cleanupDownload)
		
		// Clean up local file
		if err := os.Remove(outputEpubFile); err == nil {
			log.Printf("[Cleanup] Removed EPUB file: %s", outputEpubFile)
		}
		return
	}

	// Update status to completed
	downloadsLock.Lock()
	download.Status = "completed"
	download.Progress = 100
	download.Message = "Download complete!"
	download.FilePath = "" // Local files are deleted after upload
	download.BookTitle = bookTitle
	download.FileSize = epubFileSize
	download.EpubSize = epubFileSize
	download.MinIOURL = minioEpubURL
	download.EpubURL = minioEpubURL
	download.Timestamp = time.Now().Unix()
	downloadsLock.Unlock()
	
	// Broadcast completion to SSE clients
	download.UpdateStatus("completed", "Download complete!", 100)
	
	// Cleanup from memory after 5 minutes (enough time for client to retrieve status)
	go func() {
		time.Sleep(5 * time.Minute)
		cleanupDownload(downloadID)
	}()
}

// convertWithCalibre converts EPUB using Calibre
func convertWithCalibre(inputPath, outputPath string) error {
	args := []string{inputPath, outputPath}
	
	cmd := exec.Command("ebook-convert", args...)
	
	// Capture stderr to see conversion errors
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = nil
	
	// Set timeout
	timeout := 5 * time.Minute
	
	timer := time.AfterFunc(timeout, func() {
		cmd.Process.Kill()
	})
	defer timer.Stop()

	err := cmd.Run()
	if err != nil {
		// Log the actual error for debugging
		errorMsg := stderr.String()
		if errorMsg != "" {
			// Truncate very long error messages
			if len(errorMsg) > 500 {
				errorMsg = errorMsg[:500] + "..."
			}
			log.Printf("[Conversion] Calibre stderr: %s", errorMsg)
		}
		
		return fmt.Errorf("conversion failed: %w", err)
	}

	return nil
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
		"epub_size":  download.EpubSize,
		"cached":     download.Cached,
	}

	if download.Error != "" {
		response["error"] = download.Error
	}
	
	// Return EPUB URL
	if download.EpubURL != "" {
		response["epub_url"] = download.EpubURL
	}
	
	// Backwards compatibility
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

	// Redirect to MinIO URL (files are no longer stored locally)
	if download.MinIOURL != "" {
		http.Redirect(w, r, download.MinIOURL, http.StatusTemporaryRedirect)
		return
	}

	// No MinIO URL available - this shouldn't happen in normal operation
	log.Printf("[GetFile] ERROR: No MinIO URL for completed download %s", downloadID)
	http.Error(w, `{"error":"File not available - no storage URL found"}`, http.StatusNotFound)
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

	// Note: We don't check cache here because cache only has minimal info (title, epub path)
	// but preview needs full details (authors, description, cover, etc.)
	log.Printf("[BookInfo] Fetching full book info from O'Reilly: %s", bookID)
	
	// Create a temporary client just to fetch book info
	client, err := oreilly.NewClient(bookID, cookiesPath, nil)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"Failed to connect: %s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	// Fetch book info from O'Reilly
	if err := client.GetBookInfo(); err != nil {
		// Check if it's a "book not found" error (status 404 from API)
		if strings.Contains(err.Error(), "book not found") || strings.Contains(err.Error(), "status: 404") {
			log.Printf("[BookInfo] Book not found on O'Reilly: %s", bookID)
			http.Error(w, `{"error":"Book not found"}`, http.StatusNotFound)
			return
		}
		log.Printf("[BookInfo] Error fetching book info: %v", err)
		http.Error(w, fmt.Sprintf(`{"error":"Failed to fetch book info: %s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	bookInfo := client.GetBookInfoData()
	log.Printf("[BookInfo] Successfully fetched: %s", bookInfo.Title)
	
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

// GetStatsHandler returns server statistics and concurrency info
func GetStatsHandler(w http.ResponseWriter, r *http.Request) {
	downloadsLock.RLock()
	totalDownloads := len(downloads)
	
	var activeCount, completedCount, errorCount, queuedCount int
	for _, download := range downloads {
		switch download.Status {
		case "downloading":
			activeCount++
		case "completed":
			completedCount++
		case "error":
			errorCount++
		default:
			queuedCount++
		}
	}
	downloadsLock.RUnlock()
	
	// Get semaphore capacities
	downloadSlots := cap(downloadSemaphore)
	conversionSlots := cap(conversionSemaphore)
	
	// Get current usage
	downloadSlotsUsed := len(downloadSemaphore)
	conversionSlotsUsed := len(conversionSemaphore)
	
	stats := map[string]interface{}{
		"total_downloads":        totalDownloads,
		"active_downloads":       activeCount,
		"completed_downloads":    completedCount,
		"failed_downloads":       errorCount,
		"queued_downloads":       queuedCount,
		"download_slots_total":   downloadSlots,
		"download_slots_used":    downloadSlotsUsed,
		"download_slots_free":    downloadSlots - downloadSlotsUsed,
		"conversion_slots_total": conversionSlots,
		"conversion_slots_used":  conversionSlotsUsed,
		"conversion_slots_free":  conversionSlots - conversionSlotsUsed,
		"redis_enabled":          RedisClient != nil,
		"minio_enabled":          MinIOClient != nil,
		"presigned_url_expiry_hours": int(PresignedURLExpiry.Hours()),
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

// StreamDownloadStatusHandler handles SSE connections for real-time download progress
func StreamDownloadStatusHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	downloadID := vars["id"]
	
	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	
	// Get flusher
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}
	
	// Get download
	downloadsLock.RLock()
	download, exists := downloads[downloadID]
	downloadsLock.RUnlock()
	
	if !exists {
		// Send error event
		fmt.Fprintf(w, "data: {\"error\":\"Download ID not found\"}\n\n")
		flusher.Flush()
		return
	}
	
	// Create client channel
	client := make(chan models.DownloadUpdate, 10)
	download.AddSSEClient(client)
	defer download.RemoveSSEClient(client)
	
	// Send initial state immediately
	status, message, progress := download.GetStatus()
	initialUpdate := models.DownloadUpdate{
		Status:   status,
		Progress: progress,
		Message:  message,
	}
	
	if data, err := json.Marshal(initialUpdate); err == nil {
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}
	
	// Listen for updates or client disconnect
	ctx := r.Context()
	
	for {
		select {
		case <-ctx.Done():
			// Client disconnected
			log.Printf("[SSE] Client disconnected from download %s", downloadID)
			return
			
		case update := <-client:
			// Send update to client
			data, err := json.Marshal(update)
			if err != nil {
				log.Printf("[SSE] Error marshaling update: %v", err)
				continue
			}
			
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			
			// If completed or error, close after sending
			if update.Status == "completed" || update.Status == "error" {
				log.Printf("[SSE] Download %s finished with status: %s", downloadID, update.Status)
				return
			}
		}
	}
}

