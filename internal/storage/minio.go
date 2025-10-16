package storage

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// MinIOClient wraps the MinIO client
type MinIOClient struct {
	client     *minio.Client
	bucketName string
	useSSL     bool
	ctx        context.Context
}

// MinIOConfig holds MinIO configuration
type MinIOConfig struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
	UseSSL    bool
	Region    string
}

// NewMinIOClient creates a new MinIO client
func NewMinIOClient(config MinIOConfig) (*MinIOClient, error) {
	client, err := minio.New(config.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(config.AccessKey, config.SecretKey, ""),
		Secure: config.UseSSL,
		Region: config.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create MinIO client: %w", err)
	}

	ctx := context.Background()

	// Check if bucket exists
	exists, err := client.BucketExists(ctx, config.Bucket)
	if err != nil {
		// If we can't check bucket existence, try to continue anyway
		// (it might exist but we don't have permissions to check)
		log.Printf("[MinIO] Connected (bucket: %s, verification skipped)", config.Bucket)
	} else if !exists {
		// Only try to create if we confirmed it doesn't exist
		if err := client.MakeBucket(ctx, config.Bucket, minio.MakeBucketOptions{
			Region: config.Region,
		}); err != nil {
			return nil, fmt.Errorf("bucket does not exist and cannot be created: %w", err)
		}
		log.Printf("[MinIO] Created bucket: %s", config.Bucket)
	} else {
		log.Printf("[MinIO] Connected (bucket: %s)", config.Bucket)
	}
	return &MinIOClient{
		client:     client,
		bucketName: config.Bucket,
		useSSL:     config.UseSSL,
		ctx:        ctx,
	}, nil
}

// UploadFile uploads a file to MinIO under bookID folder
func (m *MinIOClient) UploadFile(bookID, localFilePath string) (string, int64, error) {
	// Get file info
	fileInfo, err := os.Stat(localFilePath)
	if err != nil {
		return "", 0, fmt.Errorf("failed to stat file: %w", err)
	}

	// Create object name: bookID/filename.epub
	fileName := filepath.Base(localFilePath)
	objectName := fmt.Sprintf("%s/%s", bookID, fileName)

	// Open file
	file, err := os.Open(localFilePath)
	if err != nil {
		return "", 0, fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	// Upload file
	contentType := "application/epub+zip"
	uploadInfo, err := m.client.PutObject(
		m.ctx,
		m.bucketName,
		objectName,
		file,
		fileInfo.Size(),
		minio.PutObjectOptions{
			ContentType: contentType,
		},
	)
	if err != nil {
		return "", 0, fmt.Errorf("failed to upload file: %w", err)
	}

	log.Printf("[Storage] Uploaded: %s (%.2f MB)", objectName, float64(uploadInfo.Size)/(1024*1024))
	return objectName, uploadInfo.Size, nil
}

// FileExists checks if a file exists in MinIO under bookID folder
func (m *MinIOClient) FileExists(bookID string) (bool, string, int64, error) {
	// List objects under bookID prefix
	objectCh := m.client.ListObjects(m.ctx, m.bucketName, minio.ListObjectsOptions{
		Prefix:    bookID + "/",
		Recursive: true,
	})

	for object := range objectCh {
		if object.Err != nil {
			return false, "", 0, object.Err
		}

		// Check if it's an epub file
		if filepath.Ext(object.Key) == ".epub" {
			return true, object.Key, object.Size, nil
		}
	}

	return false, "", 0, nil
}

// GetPresignedURL generates a presigned URL for downloading
func (m *MinIOClient) GetPresignedURL(objectName string, expiry time.Duration) (string, error) {
	url, err := m.client.PresignedGetObject(m.ctx, m.bucketName, objectName, expiry, nil)
	if err != nil {
		return "", fmt.Errorf("failed to generate presigned URL: %w", err)
	}
	return url.String(), nil
}

// DownloadFile downloads a file from MinIO
func (m *MinIOClient) DownloadFile(objectName, destPath string) error {
	object, err := m.client.GetObject(m.ctx, m.bucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		return fmt.Errorf("failed to get object: %w", err)
	}
	defer object.Close()

	// Create destination file
	destFile, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("failed to create destination file: %w", err)
	}
	defer destFile.Close()

	// Copy object to file
	_, err = io.Copy(destFile, object)
	if err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	return nil
}

// DeleteFile deletes a file from MinIO
func (m *MinIOClient) DeleteFile(objectName string) error {
	if err := m.client.RemoveObject(m.ctx, m.bucketName, objectName, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("failed to delete object: %w", err)
	}
	return nil
}

// GetObjectInfo gets information about an object
func (m *MinIOClient) GetObjectInfo(objectName string) (*minio.ObjectInfo, error) {
	info, err := m.client.StatObject(m.ctx, m.bucketName, objectName, minio.StatObjectOptions{})
	if err != nil {
		return nil, err
	}
	return &info, nil
}
