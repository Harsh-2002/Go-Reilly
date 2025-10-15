package main

import (
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"

	"github.com/gorilla/mux"
	"goreilly/internal/handlers"
	"github.com/rs/cors"
)

//go:embed static
var staticFS embed.FS

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	os.MkdirAll("Books", 0755)
	os.MkdirAll("Converted", 0755)

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
	log.Printf("Server starting on %s", addr)

	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatal(err)
	}
}
