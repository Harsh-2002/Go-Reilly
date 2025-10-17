package models

import (
	"sync"
	"time"
)

// BookInfo represents book metadata from O'Reilly API
type BookInfo struct {
	ID          string   `json:"id"`
	ISBN        string   `json:"isbn"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Authors     []Author `json:"authors"`
	Publishers  []Publisher `json:"publishers"`
	Subjects    []Subject `json:"subjects"`
	WebURL      string   `json:"web_url"`
	Issued      string   `json:"issued"`
	Rights      string   `json:"rights"`
	Cover       string   `json:"cover"`
}

type Author struct {
	Name string `json:"name"`
}

type Publisher struct {
	Name string `json:"name"`
}

type Subject struct {
	Name string `json:"name"`
}

// Chapter represents a book chapter
type Chapter struct {
	ID           string   `json:"id"`
	URL          string   `json:"url"`
	Filename     string   `json:"filename"`
	Title        string   `json:"title"`
	Content      string   `json:"content"`
	Images       []string `json:"images"`
	Stylesheets  []Stylesheet `json:"stylesheets"`
	SiteStyles   []string `json:"site_styles"`
	AssetBaseURL string   `json:"asset_base_url"`
}

type Stylesheet struct {
	URL string `json:"url"`
}

// TOCItem represents table of contents item
type TOCItem struct {
	ID       string    `json:"id"`
	Fragment string    `json:"fragment"`
	Href     string    `json:"href"`
	Label    string    `json:"label"`
	Depth    int       `json:"depth"`
	Children []TOCItem `json:"children"`
}

// Download represents a download job
type Download struct {
	ID         string    `json:"id"`
	BookID     string    `json:"book_id"`
	Status     string    `json:"status"`
	Progress   int       `json:"progress"`
	Message    string    `json:"message"`
	Error      string    `json:"error,omitempty"`
	FilePath   string    `json:"file_path,omitempty"`
	BookTitle  string    `json:"book_title,omitempty"`
	FileSize   int64     `json:"file_size,omitempty"`
	EpubSize   int64     `json:"epub_size,omitempty"`
	Timestamp  int64     `json:"timestamp"`
	Cached     bool      `json:"cached"`
	MinIOURL   string    `json:"minio_url,omitempty"`
	EpubURL    string    `json:"epub_url,omitempty"`
	UploadedAt time.Time `json:"uploaded_at,omitempty"`
	mutex      sync.RWMutex
	
	// SSE support
	sseClients map[chan DownloadUpdate]bool
	sseMutex   sync.RWMutex
}

// DownloadUpdate represents a status update sent via SSE
type DownloadUpdate struct {
	Status    string `json:"status"`
	Progress  int    `json:"progress"`
	Message   string `json:"message"`
	Error     string `json:"error,omitempty"`
	BookTitle string `json:"book_title,omitempty"`
	FileSize  int64  `json:"file_size,omitempty"`
	EpubSize  int64  `json:"epub_size,omitempty"`
	EpubURL   string `json:"epub_url,omitempty"`
	MinIOURL  string `json:"minio_url,omitempty"`
	Cached    bool   `json:"cached,omitempty"`
}

// UpdateStatus safely updates download status
func (d *Download) UpdateStatus(status, message string, progress int) {
	d.mutex.Lock()
	d.Status = status
	d.Message = message
	d.Progress = progress
	d.mutex.Unlock()
	
	// Broadcast to SSE clients
	d.broadcastUpdate()
}

// broadcastUpdate sends updates to all connected SSE clients
func (d *Download) broadcastUpdate() {
	d.mutex.RLock()
	update := DownloadUpdate{
		Status:    d.Status,
		Progress:  d.Progress,
		Message:   d.Message,
		Error:     d.Error,
		BookTitle: d.BookTitle,
		FileSize:  d.FileSize,
		EpubSize:  d.EpubSize,
		EpubURL:   d.EpubURL,
		MinIOURL:  d.MinIOURL,
		Cached:    d.Cached,
	}
	d.mutex.RUnlock()
	
	d.sseMutex.RLock()
	defer d.sseMutex.RUnlock()
	
	for client := range d.sseClients {
		select {
		case client <- update:
			// Successfully sent
		default:
			// Client channel is full or closed, skip
		}
	}
}

// AddSSEClient registers a new SSE client
func (d *Download) AddSSEClient(client chan DownloadUpdate) {
	d.sseMutex.Lock()
	defer d.sseMutex.Unlock()
	
	if d.sseClients == nil {
		d.sseClients = make(map[chan DownloadUpdate]bool)
	}
	d.sseClients[client] = true
}

// RemoveSSEClient unregisters an SSE client
func (d *Download) RemoveSSEClient(client chan DownloadUpdate) {
	d.sseMutex.Lock()
	defer d.sseMutex.Unlock()
	
	delete(d.sseClients, client)
	close(client)
}

// SetError safely sets error and schedules cleanup
func (d *Download) SetError(err string, cleanupFunc func(string)) {
	d.mutex.Lock()
	d.Status = "error"
	d.Error = err
	d.Message = err
	downloadID := d.ID
	d.mutex.Unlock()
	
	// Broadcast error to SSE clients
	d.broadcastUpdate()
	
	// Cleanup from memory after 2 minutes (enough time for client to see error)
	if cleanupFunc != nil {
		go func() {
			time.Sleep(2 * time.Minute)
			cleanupFunc(downloadID)
		}()
	}
}

// GetStatus safely gets status
func (d *Download) GetStatus() (string, string, int) {
	d.mutex.RLock()
	defer d.mutex.RUnlock()
	return d.Status, d.Message, d.Progress
}

// ProgressCallback is a function type for progress updates
type ProgressCallback func(stage string, progress int, message string)
