package models

import "sync"

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
	ID        string  `json:"id"`
	BookID    string  `json:"book_id"`
	Status    string  `json:"status"`
	Progress  int     `json:"progress"`
	Message   string  `json:"message"`
	Error     string  `json:"error,omitempty"`
	FilePath  string  `json:"file_path,omitempty"`
	BookTitle string  `json:"book_title,omitempty"`
	FileSize  int64   `json:"file_size,omitempty"`
	Timestamp int64   `json:"timestamp"`
	mutex     sync.RWMutex
}

// UpdateStatus safely updates download status
func (d *Download) UpdateStatus(status, message string, progress int) {
	d.mutex.Lock()
	defer d.mutex.Unlock()
	d.Status = status
	d.Message = message
	d.Progress = progress
}

// SetError safely sets error
func (d *Download) SetError(err string) {
	d.mutex.Lock()
	defer d.mutex.Unlock()
	d.Status = "error"
	d.Error = err
	d.Message = err
}

// GetStatus safely gets status
func (d *Download) GetStatus() (string, string, int) {
	d.mutex.RLock()
	defer d.mutex.RUnlock()
	return d.Status, d.Message, d.Progress
}

// ProgressCallback is a function type for progress updates
type ProgressCallback func(stage string, progress int, message string)
