// ============================================================================
// CONSTANTS AND STATE
// ============================================================================
const API_BASE = window.location.origin;

// Download state management
let activeDownloads = {};
let pollIntervals = {}; // Keep for backward compatibility fallback
let sseConnections = {}; // Store SSE connections
let lastFailedBookId = null;
let serverStatusInterval = null;
let presignedURLExpiryHours = 1; // Default, will be updated from server

// Book validation state
let bookValidationCache = {}; // Store validation results: { bookId: { valid: true/false, timestamp: Date } }

// DOM Elements
const bookIdInput = document.getElementById('book-id');
const errorDiv = document.getElementById('error');
const errorMessage = document.getElementById('error-message');
const retryBtn = document.getElementById('retry-btn');
const logoClickable = document.getElementById('logo-clickable');
const isbnValidationIndicator = document.getElementById('isbn-validation-indicator');

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

// Extract ISBN from O'Reilly URL
function extractISBNFromURL(input) {
    // If it's not a URL, return as-is
    if (!input.includes('oreilly.com') && !input.includes('http')) {
        return input;
    }
    
    // Patterns to match O'Reilly URLs:
    // 1. https://learning.oreilly.com/library/view/book-name/9781491936153/
    // 2. https://www.oreilly.com/library/view/book-name/9781491936153/
    // 3. https://learning.oreilly.com/library/view/book-name/9781491936153/ch11.html
    
    // Match 13-digit ISBN in URL path (starts with 978 or 979)
    const isbnPattern = /\/(97[89]\d{10})\/?/;
    const match = input.match(isbnPattern);
    
    if (match && match[1]) {
        console.log('[URL Detection] Extracted ISBN:', match[1]);
        return match[1];
    }
    
    // If no ISBN found in URL, return original input
    console.log('[URL Detection] No ISBN found in URL');
    return input;
}

// Show validation indicator
function showValidationIndicator(isValid) {
    if (!isbnValidationIndicator) return;
    
    // Hide loading indicator when showing validation
    hideSearchLoading();
    
    isbnValidationIndicator.classList.remove('hidden', 'valid', 'invalid');
    
    if (isValid) {
        isbnValidationIndicator.classList.add('valid');
        isbnValidationIndicator.setAttribute('aria-label', 'Valid ISBN');
    } else {
        isbnValidationIndicator.classList.add('invalid');
        isbnValidationIndicator.setAttribute('aria-label', 'Invalid ISBN');
    }
}

// Hide validation indicator
function hideValidationIndicator() {
    if (!isbnValidationIndicator) return;
    isbnValidationIndicator.classList.add('hidden');
    isbnValidationIndicator.classList.remove('valid', 'invalid');
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

// Format ISBN as user types with auto-hyphen insertion
function formatISBNAsUserTypes(value, previousValue = '') {
    // Remove all non-digits
    const cleanValue = value.replace(/[^\d]/g, '');
    
    // Don't format if empty
    if (cleanValue.length === 0) {
        return '';
    }
    
    // Apply formatting based on length
    let formatted = cleanValue;
    
    if (cleanValue.length > 3) {
        formatted = cleanValue.slice(0, 3) + '-' + cleanValue.slice(3);
    }
    if (cleanValue.length > 4) {
        formatted = cleanValue.slice(0, 3) + '-' + cleanValue.slice(3, 4) + '-' + cleanValue.slice(4);
    }
    if (cleanValue.length > 7) {
        formatted = cleanValue.slice(0, 3) + '-' + cleanValue.slice(3, 4) + '-' + cleanValue.slice(4, 7) + '-' + cleanValue.slice(7);
    }
    if (cleanValue.length > 12) {
        formatted = cleanValue.slice(0, 3) + '-' + cleanValue.slice(3, 4) + '-' + cleanValue.slice(4, 7) + '-' + cleanValue.slice(7, 12) + '-' + cleanValue.slice(12, 13);
    }
    
    return formatted;
}

// Apply formatting to input field while maintaining cursor position
function applyISBNFormatting(input) {
    const start = input.selectionStart;
    const end = input.selectionEnd;
    const previousValue = input.value;
    const formattedValue = formatISBNAsUserTypes(input.value, previousValue);
    
    // Only update if value changed
    if (formattedValue !== input.value) {
        input.value = formattedValue;
        
        // Calculate new cursor position
        // Count hyphens before cursor in old and new values
        const oldBeforeCursor = previousValue.substring(0, start);
        const oldHyphens = (oldBeforeCursor.match(/-/g) || []).length;
        const cleanBeforeCursor = oldBeforeCursor.replace(/[^\d]/g, '');
        const digitCount = cleanBeforeCursor.length;
        
        // Find position in new formatted string with same number of digits
        let newPosition = 0;
        let digitsFound = 0;
        for (let i = 0; i < formattedValue.length; i++) {
            if (formattedValue[i] !== '-') {
                digitsFound++;
            }
            if (digitsFound === digitCount) {
                newPosition = i + 1;
                break;
            }
        }
        
        // If we're at the end, just go to the end
        if (start === previousValue.length) {
            newPosition = formattedValue.length;
        }
        
        input.setSelectionRange(newPosition, newPosition);
    }
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
// SERVER STATUS MANAGEMENT
// ============================================================================

async function updateServerStatus() {
    const logoAscii = document.getElementById('logo-ascii');
    
    if (!logoAscii) return;
    
    try {
        const response = await fetch(`${API_BASE}/api/stats`);
        
        if (!response.ok) {
            throw new Error('Failed to fetch stats');
        }
        
        const stats = await response.json();
        
        // Update presigned URL expiry hours
        if (stats.presigned_url_expiry_hours) {
            presignedURLExpiryHours = stats.presigned_url_expiry_hours;
        }
        
        // Determine server health based on active downloads
        const activeDownloads = stats.active_downloads || 0;
        const downloadSlotsFree = stats.download_slots_free || 0;
        
        // Remove all status classes
        logoAscii.classList.remove('healthy', 'busy', 'error');
        
        // Update logo animation and aria-label for accessibility
        if (activeDownloads === 0) {
            // Animating colors: No active downloads, server is idle
            logoAscii.classList.add('healthy');
            logoAscii.setAttribute('title', 'Server is ready - logo colors animating');
        } else if (downloadSlotsFree > 0) {
            // Slower animation: Some downloads active but slots available
            logoAscii.classList.add('busy');
            logoAscii.setAttribute('title', `Server has ${activeDownloads} active download${activeDownloads > 1 ? 's' : ''} - logo animation slowed`);
        } else {
            // Slower animation: All slots occupied
            logoAscii.classList.add('busy');
            logoAscii.setAttribute('title', 'Server is busy with maximum downloads - logo animation slowed');
        }
        
    } catch (error) {
        console.error('[ServerStatus] Error fetching stats:', error);
        logoAscii.classList.remove('healthy', 'busy');
        logoAscii.classList.add('error');
        logoAscii.setAttribute('title', 'Server is offline - logo colors static');
    }
}


// Start server status monitoring
function startServerStatusMonitoring() {
    // Update immediately
    updateServerStatus();
    
    // Then update every 5 seconds for faster error detection
    if (serverStatusInterval) {
        clearInterval(serverStatusInterval);
    }
    serverStatusInterval = setInterval(updateServerStatus, 5000);
}

// ============================================================================
// UI STATE MANAGEMENT
// ============================================================================

function showProcessing(message = 'Processing...') {
    // Always get fresh references from DOM
    const processingStatus = document.getElementById('input-processing-status');
    const processingText = document.getElementById('input-processing-text');
    const btn = document.getElementById('input-download-btn');
    
    // Show processing in input bar
    if (processingStatus && processingText) {
        processingStatus.classList.remove('hidden');
        processingText.textContent = message;
    }
    // Hide download button
    if (btn) {
        btn.classList.add('hidden');
    }
}

function hideProcessing() {
    const processingStatus = document.getElementById('input-processing-status');
    if (processingStatus) {
        processingStatus.classList.add('hidden');
    }
}

function showDownloadReady(epubSize, epubUrl) {
    hideProcessing();
    
    // Get references for both desktop and mobile buttons
    const desktopBtn = document.getElementById('input-download-btn');
    const mobileBtn = document.getElementById('mobile-download-btn');
    const desktopSizeSpan = document.getElementById('input-epub-size');
    const mobileSizeSpan = document.getElementById('mobile-epub-size');
    
    if (epubSize) {
        console.log('[Download] Showing download buttons with size:', epubSize);
        const sizeInMB = (epubSize / (1024 * 1024)).toFixed(1);
        const sizeText = `(${sizeInMB} MB)`;
        
        // Show desktop button
        if (desktopBtn) {
            desktopBtn.classList.remove('hidden');
            desktopBtn.setAttribute('data-url', epubUrl || '');
            if (desktopSizeSpan) {
                desktopSizeSpan.textContent = sizeText;
            }
        }
        
        // Show mobile button
        if (mobileBtn) {
            mobileBtn.classList.remove('hidden');
            mobileBtn.setAttribute('data-url', epubUrl || '');
            if (mobileSizeSpan) {
                mobileSizeSpan.textContent = sizeText;
            }
        }
    } else {
        console.error('[Download] Cannot show buttons - epubSize:', epubSize);
    }
}

function hideDownloadReady() {
    const desktopBtn = document.getElementById('input-download-btn');
    const mobileBtn = document.getElementById('mobile-download-btn');
    
    if (desktopBtn) {
        desktopBtn.classList.add('hidden');
    }
    if (mobileBtn) {
        mobileBtn.classList.add('hidden');
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
        
        // Use SSE instead of polling
        connectSSE(downloadId);
        
    } catch (error) {
        showError('Unable to prepare your book. Please try again.', true);
        hideProcessing();
        hideDownloadReady();
    }
}

// Connect to SSE for real-time updates
function connectSSE(downloadId) {
    if (!downloadId || sseConnections[downloadId]) return;
    
    console.log('[SSE] Connecting to stream for:', downloadId);
    
    const eventSource = new EventSource(`${API_BASE}/api/stream/${downloadId}`);
    sseConnections[downloadId] = eventSource;
    
    eventSource.onmessage = (event) => {
        try {
            const data = JSON.parse(event.data);
            
            // Check for error in data
            if (data.error) {
                console.error('[SSE] Error from server:', data.error);
                eventSource.close();
                delete sseConnections[downloadId];
                showError('Download not found. Please try again.', true);
                return;
            }
            
            // Update active downloads
            activeDownloads[downloadId] = {
                ...activeDownloads[downloadId],
                ...data
            };
            
            updateProgress(data, downloadId);
            
            if (data.status === 'completed') {
                handleCompletion(data, downloadId);
                eventSource.close();
                delete sseConnections[downloadId];
            } else if (data.status === 'error') {
                // Improve error message based on error type
                let errorMsg = data.error || 'Unable to download this book. Please try again.';
                
                // Make error messages more user-friendly
                if (errorMsg.includes('book not found') || errorMsg.includes('Book not found')) {
                    errorMsg = 'Book not found. Please verify the ISBN number.';
                } else if (errorMsg.includes('authentication') || errorMsg.includes('cookies')) {
                    errorMsg = 'Authentication failed. Please refresh your cookies.';
                } else if (errorMsg.includes('expired')) {
                    errorMsg = 'Your O\'Reilly subscription has expired.';
                } else if (errorMsg.includes('Failed to upload')) {
                    errorMsg = 'Storage error. Please try again.';
                }
                
                showError(errorMsg, true);
                hideProcessing();
                
                // Store the failed book ID for retry
                lastFailedBookId = activeDownloads[downloadId]?.bookId || bookIdInput.value.replace(/[-\s]/g, '');
                
                delete activeDownloads[downloadId];
                eventSource.close();
                delete sseConnections[downloadId];
                
                // Clear the input after showing error so user can try another book
                setTimeout(() => {
                    bookIdInput.value = '';
                }, 100);
            }
        } catch (e) {
            console.error('[SSE] Error parsing message:', e);
        }
    };
    
    eventSource.onerror = (error) => {
        console.error('[SSE] Connection error:', error);
        eventSource.close();
        delete sseConnections[downloadId];
        
        // Fallback to polling if SSE fails
        console.log('[SSE] Falling back to polling for:', downloadId);
        pollStatus(downloadId);
    };
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
            // Improve error message
            let errorMsg = data.error || 'Unable to download this book. Please try again.';
            
            if (errorMsg.includes('book not found') || errorMsg.includes('Book not found')) {
                errorMsg = 'Book not found. Please verify the ISBN number.';
            } else if (errorMsg.includes('authentication') || errorMsg.includes('cookies')) {
                errorMsg = 'Authentication failed. Please refresh your cookies.';
            }
            
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
    
    // Check if this is a cached book
    const isCached = data.cached === true;
    
    try {
        let fileInfo;
        
        // If we have epub_size in the SSE data, use it directly (first download)
        if (data.epub_size || data.file_size) {
            console.log('[Download] Using size from SSE data:', data.epub_size || data.file_size);
            fileInfo = {
                title: data.book_title || 'Unknown Title',
                epub_size: data.epub_size || data.file_size,
                book_id: data.book_id,
                cached: isCached,
                epub_url: data.epub_url || data.minio_url,
                minio_url: data.minio_url || data.epub_url,
                uploaded_at: data.uploaded_at
            };
        } else if (isCached) {
            // Cached book without size in SSE, use data directly
            fileInfo = {
                title: data.book_title || 'Unknown Title',
                epub_size: data.epub_size || 0,
                book_id: data.book_id,
                cached: true,
                epub_url: data.epub_url,
                uploaded_at: data.uploaded_at
            };
        } else {
            // Fallback: Fetch file info from API (shouldn't happen normally)
            console.log('[Download] Fetching file info from API as fallback');
            const fileInfoResponse = await fetch(`${API_BASE}/api/file/${downloadId}/info`);
            if (!fileInfoResponse.ok) {
                throw new Error('Unable to retrieve book information');
            }
            fileInfo = await fileInfoResponse.json();
        }
        
        activeDownloads[downloadId].fileInfo = fileInfo;
        
        // Always show download button for completed downloads
        console.log('[Download] Showing download button with size:', fileInfo.epub_size);
        showDownloadReady(
            fileInfo.epub_size || fileInfo.file_size,
            fileInfo.epub_url || fileInfo.minio_url
        );
        setupDownloadButtons(fileInfo, downloadId);
        
    } catch (error) {
        console.error(`Error getting file info for ${downloadId}:`, error);
        delete activeDownloads[downloadId];
        showError('Unable to prepare your book for download. Please try again.', true);
        hideProcessing();
    }
}

function setupDownloadButtons(fileInfo, downloadId) {
    const desktopBtn = document.getElementById('input-download-btn');
    const mobileBtn = document.getElementById('mobile-download-btn');
    
    if (!desktopBtn && !mobileBtn) {
        console.error('[Download] Button elements not found!');
        return;
    }
    
    console.log('[Download] Setting up download buttons');
    
    const downloadUrl = fileInfo.epub_url || fileInfo.minio_url || `${API_BASE}/api/file/${downloadId}`;
    
    // Setup desktop button
    if (desktopBtn) {
        const newDesktopBtn = desktopBtn.cloneNode(true);
        desktopBtn.parentNode.replaceChild(newDesktopBtn, desktopBtn);
        
        newDesktopBtn.addEventListener('click', () => {
            console.log('[Download] Opening:', downloadUrl);
            window.open(downloadUrl, '_blank');
        });
        
        newDesktopBtn.classList.remove('hidden');
    }
    
    // Setup mobile button
    if (mobileBtn) {
        const newMobileBtn = mobileBtn.cloneNode(true);
        mobileBtn.parentNode.replaceChild(newMobileBtn, mobileBtn);
        
        newMobileBtn.addEventListener('click', () => {
            console.log('[Download] Opening:', downloadUrl);
            window.open(downloadUrl, '_blank');
        });
        
        newMobileBtn.classList.remove('hidden');
    }
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

// Show quick title preview
function showQuickTitlePreview(title) {
    const quickPreview = document.getElementById('quick-title-preview');
    const quickTitleText = document.getElementById('quick-title-text');
    
    if (quickPreview && quickTitleText && title) {
        quickTitleText.textContent = title;
        quickPreview.classList.remove('hidden');
    }
}

// Hide quick title preview
function hideQuickTitlePreview() {
    const quickPreview = document.getElementById('quick-title-preview');
    if (quickPreview) {
        quickPreview.classList.add('hidden');
    }
}

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
            hideQuickTitlePreview();
            
            // Mark this book as invalid in cache
            bookValidationCache[bookId] = { valid: false, timestamp: Date.now() };
            
            if (response.status === 404) {
                showError('Book not found. Please check the ISBN number and try again.', false);
            } else if (response.status === 401 || response.status === 403) {
                showError('Authentication error. Please refresh your O\'Reilly cookies.', false);
            } else if (response.status >= 500) {
                showError('Server error. Please try again in a moment.', false);
            } else {
                showError('Unable to retrieve book information. Please try again.', false);
            }
            previewDiv.classList.add('hidden');
            return;
        }
        
        const bookInfo = await response.json();
        
        // Mark this book as valid in cache
        bookValidationCache[bookId] = { valid: true, timestamp: Date.now() };
        
        // Show quick title preview immediately
        if (bookInfo.title) {
            showQuickTitlePreview(bookInfo.title);
        }
        
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
        hideQuickTitlePreview();
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
    hideValidationIndicator();
    hideQuickTitlePreview();
    
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
    
    // Start server status monitoring
    startServerStatusMonitoring();
});

// Cleanup SSE connections on page unload
window.addEventListener('beforeunload', () => {
    Object.values(sseConnections).forEach(eventSource => {
        eventSource.close();
    });
});

// Debounce timer for auto-trigger
let autoDownloadTimer;

// Input change listener - auto trigger download after user stops typing
bookIdInput.addEventListener('input', (e) => {
    let inputValue = e.target.value.trim();
    
    // Extract ISBN from URL if it's a URL
    const extractedISBN = extractISBNFromURL(inputValue);
    
    // If we extracted an ISBN from URL, update the input field
    if (extractedISBN !== inputValue && extractedISBN.match(/^\d{13}$/)) {
        e.target.value = extractedISBN;
        inputValue = extractedISBN;
        console.log('[URL Detection] Updated input with extracted ISBN:', extractedISBN);
    }
    
    // Apply auto-formatting
    applyISBNFormatting(e.target);
    
    const bookId = e.target.value.trim();
    const cleanBookId = bookId.replace(/[-\s]/g, ''); // Remove hyphens for validation
    
    // Clear any existing timers
    clearTimeout(autoDownloadTimer);
    
    // Clear validation cache for this book when user modifies input
    if (bookValidationCache[cleanBookId]) {
        delete bookValidationCache[cleanBookId];
    }
    
    // Always reset UI when input changes
    resetUI();
    
    // Hide preview and loading initially
    const previewDiv = document.getElementById('book-preview');
    
    // If input is empty, hide everything
    if (cleanBookId.length === 0) {
        previewDiv.classList.add('hidden');
        hideSearchLoading();
        hideError();
        hideValidationIndicator();
        return;
    }
    
    // Show indicators based on input length and validation state
    if (cleanBookId.length > 0 && cleanBookId.length < 10) {
        // Less than 10 digits - show loading if typing
        if (cleanBookId.length >= 3) {
            showSearchLoading();
            hideValidationIndicator();
        } else {
            hideSearchLoading();
            hideValidationIndicator();
        }
    } else if (cleanBookId.length >= 10 && cleanBookId.length < 13) {
        // 10-12 digits - show loading while user finishes typing
        showSearchLoading();
        hideValidationIndicator();
    } else if (cleanBookId.length === 13) {
        // Exactly 13 digits - hide loading, show validation
        hideSearchLoading();
        const isValid = isValidISBN13(cleanBookId);
        showValidationIndicator(isValid);
        
        // Validate ISBN-13 format
        if (!isValid) {
            showError('Please enter a valid 13-digit ISBN', false);
            previewDiv.classList.add('hidden');
            return;
        }
        
        // Valid ISBN - fetch preview
        debounce(() => fetchBookPreview(cleanBookId), 800);
        
        // Auto-trigger download after 2 seconds of no typing (only if valid and book exists)
        autoDownloadTimer = setTimeout(() => {
            const currentClean = bookIdInput.value.replace(/[-\s]/g, '');
            if (currentClean === cleanBookId && isValidISBN13(cleanBookId)) {
                // Check if we already know this book doesn't exist
                const validation = bookValidationCache[cleanBookId];
                if (validation && validation.valid === false) {
                    console.log('[AutoDownload] Skipping download - book not found in preview');
                    return;
                }
                
                hideError();
                hideDownloadReady();
                hideProcessing();
                document.body.removeAttribute('data-current-download');
                startDownload(cleanBookId);
            }
        }, 2000);
    } else if (cleanBookId.length > 13) {
        // Too many digits
        hideSearchLoading();
        hideValidationIndicator();
        showError('ISBN should be exactly 13 digits', false);
        previewDiv.classList.add('hidden');
    }
});

// Also fetch preview on blur
bookIdInput.addEventListener('blur', (e) => {
    const cleanBookId = e.target.value.replace(/[-\s]/g, '');
    if (cleanBookId.length === 13 && isValidISBN13(cleanBookId)) {
        fetchBookPreview(cleanBookId);
    }
});

// Handle paste events - extract ISBN from URLs immediately
bookIdInput.addEventListener('paste', (e) => {
    // Small delay to let the paste complete
    setTimeout(() => {
        const pastedValue = e.target.value.trim();
        const extractedISBN = extractISBNFromURL(pastedValue);
        
        // If we extracted an ISBN from a URL, update the field
        if (extractedISBN !== pastedValue && extractedISBN.match(/^\d{13}$/)) {
            e.target.value = extractedISBN;
            console.log('[Paste] Extracted ISBN from URL:', extractedISBN);
            
            // Trigger input event to process the ISBN
            e.target.dispatchEvent(new Event('input', { bubbles: true }));
        }
    }, 10);
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

// Logo keyboard support for accessibility
logoClickable.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' || e.key === ' ') {
        e.preventDefault();
        toggleTheme();
    }
});

// Keyboard shortcuts
document.addEventListener('keydown', handleKeyboardShortcuts);
