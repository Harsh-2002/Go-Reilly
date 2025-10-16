package main

import (
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"

	"github.com/gorilla/mux"
	"github.com/rs/cors"
	"goreilly/internal/cache"
	"goreilly/internal/config"
	"goreilly/internal/handlers"
	"goreilly/internal/storage"
)

//go:embed static
var staticFS embed.FS

func main() {
	// Load configuration
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	port := cfg.Port

	os.MkdirAll("Books", 0755)
	os.MkdirAll("Converted", 0755)

	// Initialize Redis client
	redisClient, err := cache.NewRedisClient(cfg.RedisHost, cfg.RedisPort, cfg.RedisPassword)
	if err != nil {
		log.Printf("WARNING: Redis unavailable - %v", err)
	} else {
		handlers.RedisClient = redisClient
		defer redisClient.Close()
	}

	// Initialize MinIO client
	minioClient, err := storage.NewMinIOClient(storage.MinIOConfig{
		Endpoint:  cfg.MinIOEndpoint,
		AccessKey: cfg.MinIOAccessKey,
		SecretKey: cfg.MinIOSecretKey,
		Bucket:    cfg.MinIOBucket,
		UseSSL:    cfg.MinIOUseSSL,
		Region:    cfg.MinIORegion,
	})
	if err != nil {
		log.Printf("WARNING: MinIO unavailable - %v", err)
	} else {
		handlers.MinIOClient = minioClient
	}

	router := mux.NewRouter()

	router.HandleFunc("/api/download", handlers.DownloadBookHandler).Methods("POST")
	router.HandleFunc("/api/book/{id}/info", handlers.GetBookInfoHandler).Methods("GET")
	router.HandleFunc("/api/status/{id}", handlers.GetStatusHandler).Methods("GET")
	router.HandleFunc("/api/file/{id}", handlers.GetFileHandler).Methods("GET")
	router.HandleFunc("/api/file/{id}/info", handlers.GetFileInfoHandler).Methods("GET")

	staticContent, _ := fs.Sub(staticFS, "static")
	router.PathPrefix("/").Handler(http.FileServer(http.FS(staticContent)))

	c := cors.New(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"*"},
		AllowCredentials: true,
	})

	handler := c.Handler(router)

	addr := fmt.Sprintf("0.0.0.0:%s", port)
	log.Printf("Server started on http://localhost:%s", port)

	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatal(err)
	}
}
