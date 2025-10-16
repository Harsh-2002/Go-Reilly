package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisClient wraps the Redis client
type RedisClient struct {
	client *redis.Client
	ctx    context.Context
}

// BookCacheInfo stores cached book information
type BookCacheInfo struct {
	BookID      string    `json:"book_id"`
	BookTitle   string    `json:"book_title"`
	MinIOPath   string    `json:"minio_path"`
	FileSize    int64     `json:"file_size"`
	UploadedAt  time.Time `json:"uploaded_at"`
	ISBN        string    `json:"isbn,omitempty"`
}

// NewRedisClient creates a new Redis client
func NewRedisClient(host, port, password string) (*RedisClient, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     fmt.Sprintf("%s:%s", host, port),
		Password: password,
		DB:       0,
	})

	ctx := context.Background()

	// Test connection
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	log.Printf("[Redis] Connected successfully")
	return &RedisClient{
		client: client,
		ctx:    ctx,
	}, nil
}

// GetBookInfo retrieves cached book information
func (r *RedisClient) GetBookInfo(bookID string) (*BookCacheInfo, error) {
	key := fmt.Sprintf("book:%s", bookID)
	
	data, err := r.client.Get(r.ctx, key).Result()
	if err == redis.Nil {
		return nil, nil // Not found
	}
	if err != nil {
		return nil, err
	}

	var info BookCacheInfo
	if err := json.Unmarshal([]byte(data), &info); err != nil {
		return nil, err
	}

	log.Printf("[Cache] Found: %s", info.BookTitle)
	return &info, nil
}

// SetBookInfo stores book information in cache
func (r *RedisClient) SetBookInfo(info *BookCacheInfo) error {
	key := fmt.Sprintf("book:%s", info.BookID)
	
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}

	// Set with no expiration (or set expiration as needed)
	if err := r.client.Set(r.ctx, key, data, 0).Err(); err != nil {
		return err
	}

	log.Printf("[Cache] Stored: %s", info.BookTitle)
	return nil
}

// DeleteBookInfo removes book information from cache
func (r *RedisClient) DeleteBookInfo(bookID string) error {
	key := fmt.Sprintf("book:%s", bookID)
	return r.client.Del(r.ctx, key).Err()
}

// BookExists checks if a book exists in cache
func (r *RedisClient) BookExists(bookID string) (bool, error) {
	key := fmt.Sprintf("book:%s", bookID)
	exists, err := r.client.Exists(r.ctx, key).Result()
	if err != nil {
		return false, err
	}
	return exists > 0, nil
}

// Close closes the Redis connection
func (r *RedisClient) Close() error {
	return r.client.Close()
}
