// ============================================================================
// CONSTANTS AND STATE
// ============================================================================
const API_BASE = window.location.origin;

// Download state management
let activeDownloads = {};
let pollIntervals = {};
let lastFailedBookId = null;

// DOM Elements
const bookIdInput = document.getElementById('book-id');
const errorDiv = document.getElementById('error');
const errorMessage = document.getElementById('error-message');
const retryBtn = document.getElementById('retry-btn');
const logoClickable = document.getElementById('logo-clickable');

// New input bar elements
const inputProcessingStatus = document.getElementById('input-processing-status');
const inputProcessingText = document.getElementById('input-processing-text');
const inputDownloadBtn = document.getElementById('input-download-btn');
const inputEpubSize = document.getElementById('input-epub-size');

// ============================================================================
// UTILITY FUNCTIONS
// ============================================================================

// Validate ISBN-13 format (13 digits)
function isValidISBN13(isbn) {
    // Remove any spaces or hyphens
    const cleanISBN = isbn.replace(/[-\s]/g, '');
    
    // Check if it's exactly 13 digits
    if (!/^\d{13}$/.test(cleanISBN)) {
        return false;
    }
    
    return true;
}

// Format ISBN for display
function formatISBN(isbn) {
    const cleanISBN = isbn.replace(/[-\s]/g, '');
    if (cleanISBN.length === 13) {
        // Format as: 978-1-234-56789-0
        return `${cleanISBN.slice(0, 3)}-${cleanISBN.slice(3, 4)}-${cleanISBN.slice(4, 7)}-${cleanISBN.slice(7, 12)}-${cleanISBN.slice(12)}`;
    }
    return isbn;
}

// ============================================================================
// THEME MANAGEMENT
// ============================================================================

// Theme management - using data attribute only (no persistence)
function toggleTheme() {
    const currentTheme = document.body.getAttribute('data-theme');
    const newTheme = currentTheme === 'dark' ? 'light' : 'dark';
    document.body.setAttribute('data-theme', newTheme);
}

// ============================================================================
// UI STATE MANAGEMENT
// ============================================================================

function showProcessing(message = 'Processing...') {
    // Show processing in input bar
    if (inputProcessingStatus) {
        inputProcessingStatus.classList.remove('hidden');
        inputProcessingText.textContent = message;
    }
    // Hide download button
    if (inputDownloadBtn) {
        inputDownloadBtn.classList.add('hidden');
    }
}

function hideProcessing() {
    if (inputProcessingStatus) {
        inputProcessingStatus.classList.add('hidden');
    }
}

function showDownloadReady(epubSize, epubUrl) {
    hideProcessing();
    
    // Show download button in input bar
    if (inputDownloadBtn && epubSize) {
        inputDownloadBtn.classList.remove('hidden');
        const sizeInMB = (epubSize / (1024 * 1024)).toFixed(1);
        inputEpubSize.textContent = `(${sizeInMB} MB)`;
        inputDownloadBtn.setAttribute('data-url', epubUrl || '');
    }
}

function hideDownloadReady() {
    if (inputDownloadBtn) {
        inputDownloadBtn.classList.add('hidden');
    }
}

function showError(message, showRetry = false) {
    errorMessage.textContent = message;
    errorDiv.classList.remove('hidden');
    
    if (showRetry) {
        retryBtn.classList.remove('hidden');
    } else {
        retryBtn.classList.add('hidden');
    }
}

function hideError() {
    errorDiv.classList.add('hidden');
    retryBtn.classList.add('hidden');
}

// ============================================================================
// DOWNLOAD MANAGEMENT
// ============================================================================

async function startDownload(bookId) {
    try {
        showProcessing('Preparing your book...');
        hideError();
        hideDownloadReady();
        
        const response = await fetch(`${API_BASE}/api/download`, {
            method: 'POST',
            headers: {
                'Content-Type': 'application/json',
            },
            body: JSON.stringify({ book_id: bookId })
        });
        
        if (!response.ok) {
            const error = await response.json();
            throw new Error(error.error || 'Failed to start download');
        }
        
        const data = await response.json();
        const downloadId = data.download_id;
        
        // Check if this is a cached book (immediate response)
        if (data.cached === true && data.epub_url && data.epub_size) {
            // Cached book - show download button immediately
            activeDownloads[downloadId] = {
                status: 'completed',
                book_id: bookId,
                progress: 100,
                cached: true,
                epub_url: data.epub_url,
                epub_size: data.epub_size,
                file_size: data.file_size || data.epub_size,
                book_title: data.book_title
            };
            
            
            // Show download button immediately
            showDownloadReady(data.epub_size, data.epub_url);
            setupDownloadButtons({
                epub_url: data.epub_url,
                minio_url: data.minio_url || data.epub_url,
                epub_size: data.epub_size,
                file_size: data.file_size || data.epub_size
            }, downloadId);
            
            return; // Don't poll for cached books
        }
        
        // Not cached - normal download flow
        activeDownloads[downloadId] = {
            status: 'starting',
            book_id: bookId,
            progress: 0
        };
        
        lastFailedBookId = bookId;
        pollStatus(downloadId);
        
    } catch (error) {
        showError('Unable to prepare your book. Please try again.', true);
        hideProcessing();
        hideDownloadReady();
    }
}

async function pollStatus(downloadId) {
    if (!downloadId || !activeDownloads[downloadId]) return;
    
    try {
        const response = await fetch(`${API_BASE}/api/status/${downloadId}`);
        
        if (!response.ok) {
            throw new Error('Failed to get status');
        }
        
        const data = await response.json();
        
        activeDownloads[downloadId] = {
            ...activeDownloads[downloadId],
            ...data
        };
        
        updateProgress(data, downloadId);
        
        if (data.status === 'completed') {
            handleCompletion(data, downloadId);
        } else if (data.status === 'error') {
            const errorMsg = 'Unable to retrieve this book. Please try again or check the book number.';
            showError(errorMsg, true);
            hideProcessing();
            
            // Store the failed book ID for retry
            lastFailedBookId = activeDownloads[downloadId]?.bookId || bookIdInput.value.trim();
            
            if (pollIntervals[downloadId]) {
                clearTimeout(pollIntervals[downloadId]);
                delete pollIntervals[downloadId];
            }
            delete activeDownloads[downloadId];
            
            // Clear the input after showing error so user can try another book
            setTimeout(() => {
                bookIdInput.value = '';
            }, 100);
        } else {
            pollIntervals[downloadId] = setTimeout(() => pollStatus(downloadId), 1000);
        }
        
    } catch (error) {
        console.error(`Error polling status for ${downloadId}:`, error);
        
        if (activeDownloads[downloadId].errorCount) {
            activeDownloads[downloadId].errorCount++;
        } else {
            activeDownloads[downloadId].errorCount = 1;
        }
        
        // Increased tolerance to 20 retries instead of 10
        if (activeDownloads[downloadId].errorCount > 20) {
            showError('Connection issue. Please check your internet and try again.', true);
            hideProcessing();
            
            if (pollIntervals[downloadId]) {
                clearTimeout(pollIntervals[downloadId]);
                delete pollIntervals[downloadId];
            }
            delete activeDownloads[downloadId];
        } else {
            // Retry with exponential backoff
            const retryDelay = Math.min(2000 * activeDownloads[downloadId].errorCount, 10000);
            pollIntervals[downloadId] = setTimeout(() => pollStatus(downloadId), retryDelay);
        }
    }
}

function updateProgress(data, downloadId) {
    // Map technical messages to user-friendly ones
    let message = 'Preparing your book...';
    
    if (data.message) {
        const msg = data.message.toLowerCase();
        if (msg.includes('download')) {
            message = 'Retrieving book...';
        } else if (msg.includes('convert') || msg.includes('process')) {
            message = 'Preparing book...';
        } else if (msg.includes('upload') || msg.includes('sav')) {
            message = 'Almost ready...';
        } else if (msg.includes('complet')) {
            message = 'Ready!';
        } else {
            message = 'Processing...';
        }
    }
    
    const visibleDownloadId = document.body.getAttribute('data-current-download');
    
    if (!visibleDownloadId || visibleDownloadId === downloadId) {
        showProcessing(message);
        document.body.setAttribute('data-current-download', downloadId);
    }
    
    if (data.metadata) {
        activeDownloads[downloadId].metadata = data.metadata;
    }
}

async function handleCompletion(data, downloadId) {
    if (pollIntervals[downloadId]) {
        clearTimeout(pollIntervals[downloadId]);
        delete pollIntervals[downloadId];
    }
    
    const visibleDownloadId = document.body.getAttribute('data-current-download');
    
    // Check if this is a cached book
    const isCached = data.cached === true;
    
    try {
        let fileInfo;
        
        // If cached, use the data directly
        if (isCached) {
            fileInfo = {
                title: data.book_title || 'Unknown Title',
                epub_size: data.epub_size || 0,
                book_id: data.book_id,
                cached: true,
                epub_url: data.epub_url,
                uploaded_at: data.uploaded_at
            };
        } else {
            // Fetch file info from API
            const fileInfoResponse = await fetch(`${API_BASE}/api/file/${downloadId}/info`);
            if (!fileInfoResponse.ok) {
                throw new Error('Unable to retrieve book information');
            }
            fileInfo = await fileInfoResponse.json();
        }
        
        activeDownloads[downloadId].fileInfo = fileInfo;
        
        if (!visibleDownloadId || visibleDownloadId === downloadId) {
            setTimeout(() => {
                showDownloadReady(
                    fileInfo.epub_size || fileInfo.file_size,
                    fileInfo.epub_url || fileInfo.minio_url
                );
                setupDownloadButtons(fileInfo, downloadId);
            }, 300);
        }
        
    } catch (error) {
        console.error(`Error getting file info for ${downloadId}:`, error);
        delete activeDownloads[downloadId];
        
        if (!visibleDownloadId || visibleDownloadId === downloadId) {
            showError('Unable to prepare your book for download. Please try again.', true);
        }
    }
}

function setupDownloadButtons(fileInfo, downloadId) {
    if (!inputDownloadBtn) return;
    
    // Setup download button in input bar
    const newBtn = inputDownloadBtn.cloneNode(true);
    inputDownloadBtn.parentNode.replaceChild(newBtn, inputDownloadBtn);
    
    document.getElementById('input-download-btn').addEventListener('click', () => {
        const downloadUrl = fileInfo.epub_url || fileInfo.minio_url || `${API_BASE}/api/file/${downloadId}`;
        window.open(downloadUrl, '_blank');
    });
}

// ============================================================================
// KEYBOARD SHORTCUTS
// ============================================================================

function handleKeyboardShortcuts(e) {
    // Ctrl/Cmd + K to focus input
    if ((e.ctrlKey || e.metaKey) && e.key === 'k') {
        e.preventDefault();
        bookIdInput.focus();
        bookIdInput.select();
    }
    
    // Escape to clear input or close preview
    if (e.key === 'Escape') {
        if (bookIdInput.value) {
            bookIdInput.value = '';
            bookIdInput.blur();
        } else {
            const previewDiv = document.getElementById('book-preview');
            if (previewDiv && !previewDiv.classList.contains('hidden')) {
                previewDiv.classList.add('hidden');
                hideDownloadReady();
                hideProcessing();
            }
            hideError();
        }
    }
}

// ============================================================================
// BOOK PREVIEW
// ============================================================================

// Display book info in the preview section
function displayBookInfo(bookInfo, previewCover, previewTitle, previewAuthors, 
                        previewPublisher, previewYear, previewDescription) {
    // Set cover image
    if (bookInfo.cover) {
        previewCover.src = bookInfo.cover;
        previewCover.style.display = 'block';
    } else {
        previewCover.style.display = 'none';
    }
    
    // Set title
    previewTitle.textContent = bookInfo.title || 'Unknown Title';
    
    // Set authors
    if (bookInfo.authors && bookInfo.authors.length > 0) {
        previewAuthors.textContent = 'BY ' + bookInfo.authors.join(', ').toUpperCase();
    } else {
        previewAuthors.textContent = 'AUTHOR UNKNOWN';
    }
    
    // Set publisher
    if (bookInfo.publishers && bookInfo.publishers.length > 0) {
        previewPublisher.textContent = bookInfo.publishers.join(', ');
    } else {
        previewPublisher.textContent = '';
    }
    
    // Set year
    if (bookInfo.issued) {
        const year = new Date(bookInfo.issued).getFullYear();
        previewYear.textContent = year;
    } else {
        previewYear.textContent = '';
    }
    
    // Set description
    if (bookInfo.description) {
        // Clean and format HTML description
        let description = bookInfo.description;
        
        // Remove wrapping span/div tags if present
        description = description.replace(/^<span><div>|<\/div><\/span>$/g, '');
        
        // Convert HTML to formatted text
        const tempDiv = document.createElement('div');
        tempDiv.innerHTML = description;
        
        // Replace paragraph tags with line breaks
        const paragraphs = tempDiv.querySelectorAll('p');
        let formattedText = '';
        
        paragraphs.forEach((p, index) => {
            formattedText += p.textContent.trim();
            if (index < paragraphs.length - 1) {
                formattedText += '\n\n';
            }
        });
        
        // Handle list items
        const listItems = tempDiv.querySelectorAll('li');
        if (listItems.length > 0) {
            formattedText += '\n\n';
            listItems.forEach(li => {
                formattedText += '• ' + li.textContent.trim() + '\n';
            });
        }
        
        // If no formatted text, just get plain text
        if (!formattedText.trim()) {
            formattedText = tempDiv.textContent || tempDiv.innerText || '';
        }
        
        previewDescription.textContent = formattedText.trim();
    } else {
        previewDescription.textContent = 'No description available.';
    }
}

// Fetch and display book info preview
async function fetchBookPreview(bookId) {
    const previewDiv = document.getElementById('book-preview');
    const previewCover = document.getElementById('preview-cover');
    const previewTitle = document.getElementById('preview-title');
    const previewAuthors = document.getElementById('preview-authors');
    const previewPublisher = document.getElementById('preview-publisher');
    const previewYear = document.getElementById('preview-year');
    const previewDescription = document.getElementById('preview-description');
    
    // Don't hide if already visible - prevents flickering
    const wasHidden = previewDiv.classList.contains('hidden');
    
    if (!bookId || bookId.length !== 13) {
        previewDiv.classList.add('hidden');
        hideSearchLoading();
        return;
    }
    
    // Validate ISBN
    if (!isValidISBN13(bookId)) {
        previewDiv.classList.add('hidden');
        hideSearchLoading();
        showError('Please enter a valid 13-digit book number', false);
        return;
    }
    
    // Show loading indicator
    showSearchLoading();
    
    // Check if we have this book in recent searches
    if (recentBookSearches[bookId]) {
        // Use cached book info
        const bookInfo = recentBookSearches[bookId];
        displayBookInfo(bookInfo, previewCover, previewTitle, previewAuthors, 
            previewPublisher, previewYear, previewDescription);
        
        if (wasHidden) {
            previewDiv.classList.remove('hidden');
        }
        hideSearchLoading();
        return;
    }
    
    try {
        const response = await fetch(`${API_BASE}/api/book/${bookId}/info`);
        
        if (!response.ok) {
            hideSearchLoading();
            if (response.status === 404) {
                showError('Book not found. Please check the book number and try again.', false);
            } else {
                showError('Unable to retrieve book information. Please try again.', false);
            }
            previewDiv.classList.add('hidden');
            return;
        }
        
        const bookInfo = await response.json();
        
        // Cache this book info for future use
        recentBookSearches[bookId] = bookInfo;
        
        // Display the book information
        displayBookInfo(bookInfo, previewCover, previewTitle, previewAuthors, 
            previewPublisher, previewYear, previewDescription);
        
        // Show preview only if it was hidden before
        if (wasHidden) {
            previewDiv.classList.remove('hidden');
        }
        
        // Hide loading indicator after success
        hideSearchLoading();
        
    } catch (error) {
        console.error('Error fetching book preview:', error);
        hideSearchLoading();
    }
}

// Debounce function to limit API calls
let debounceTimer;
function debounce(func, delay) {
    clearTimeout(debounceTimer);
    debounceTimer = setTimeout(func, delay);
}

// Recent book searches cache
const recentBookSearches = {};

// Loading indicator management
function showSearchLoading() {
    const loadingIndicator = document.getElementById('search-loading-indicator');
    if (loadingIndicator) {
        loadingIndicator.classList.remove('hidden');
    }
}

function hideSearchLoading() {
    const loadingIndicator = document.getElementById('search-loading-indicator');
    if (loadingIndicator) {
        loadingIndicator.classList.add('hidden');
    }
}

// Reset UI to initial state
function resetUI() {
    // Hide all components
    hideError();
    hideDownloadReady();
    hideProcessing();
    
    // Clear current download info
    document.body.removeAttribute('data-current-download');
    
    // Reset preview content if visible
    const previewTitle = document.getElementById('preview-title');
    const previewAuthors = document.getElementById('preview-authors');
    const previewPublisher = document.getElementById('preview-publisher');
    const previewYear = document.getElementById('preview-year');
    const previewDescription = document.getElementById('preview-description');
    
    if (previewTitle) previewTitle.textContent = 'Loading...';
    if (previewAuthors) previewAuthors.textContent = 'Authors';
    if (previewPublisher) previewPublisher.textContent = '';
    if (previewYear) previewYear.textContent = '';
    if (previewDescription) previewDescription.textContent = '';
}

// ============================================================================
// EVENT LISTENERS
// ============================================================================

// Initialize on DOM load
document.addEventListener('DOMContentLoaded', function() {
    // Theme always starts as dark (no persistence)
    document.body.setAttribute('data-theme', 'dark');
    
    // Clear any stale state on fresh page load
    hideDownloadReady();
    hideProcessing();
    hideError();
    
    // Clear input field on page load (fresh start)
    if (bookIdInput) {
        bookIdInput.value = '';
    }
    
    // Reset active downloads
    activeDownloads = {};
});

// Debounce timer for auto-trigger
let autoDownloadTimer;

// Input change listener - auto trigger download after user stops typing
bookIdInput.addEventListener('input', (e) => {
    const bookId = e.target.value.trim();
    
    // Clear any existing timers
    clearTimeout(autoDownloadTimer);
    
    // Always reset UI when input changes
    resetUI();
    
    // Hide preview and loading initially
    const previewDiv = document.getElementById('book-preview');
    
    // If input is empty, hide everything
    if (bookId.length === 0) {
        previewDiv.classList.add('hidden');
        hideSearchLoading();
        hideError();
        return;
    }
    
    // Show loading indicator as soon as user starts typing (at least 3 characters)
    if (bookId.length >= 3) {
        showSearchLoading();
    } else {
        hideSearchLoading();
    }
    
    // Check if we have exactly 13 digits
    if (bookId.length === 13) {
        // Validate ISBN-13 format
        if (!isValidISBN13(bookId)) {
            hideSearchLoading();
            showError('Please enter a valid 13-digit book number', false);
            previewDiv.classList.add('hidden');
            return;
        }
        
        // Valid ISBN - fetch preview
        debounce(() => fetchBookPreview(bookId), 800);
        
        // Auto-trigger download after 2 seconds of no typing
        autoDownloadTimer = setTimeout(() => {
            if (bookIdInput.value.trim() === bookId) {
                hideError();
                hideDownloadReady();
                hideProcessing();
                document.body.removeAttribute('data-current-download');
                startDownload(bookId);
            }
        }, 2000);
    } else if (bookId.length > 13) {
        // Too many digits
        hideSearchLoading();
        showError('Book number should be exactly 13 digits', false);
        previewDiv.classList.add('hidden');
    } else if (bookId.length > 0 && bookId.length < 13) {
        // Not enough digits - show subtle hint
        hideSearchLoading();
        previewDiv.classList.add('hidden');
        // Don't show error for incomplete input, just wait for user to finish
    }
});

// Also fetch preview on blur
bookIdInput.addEventListener('blur', (e) => {
    const bookId = e.target.value.trim();
    if (bookId.length === 13 && isValidISBN13(bookId)) {
        fetchBookPreview(bookId);
    }
});

// Retry button
retryBtn.addEventListener('click', () => {
    if (lastFailedBookId) {
        bookIdInput.value = lastFailedBookId;
        startDownload(lastFailedBookId);
    }
});

// Logo click for theme toggle
logoClickable.addEventListener('click', toggleTheme);

// Keyboard shortcuts
document.addEventListener('keydown', handleKeyboardShortcuts);
