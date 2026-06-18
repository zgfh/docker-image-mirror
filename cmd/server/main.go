package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"docker-image-mirror/internal/proxy"
	"docker-image-mirror/internal/storage"
)

func main() {
	// Initialize storage
	stor, err := storage.NewLocalStorage("/var/lib/docker-mirror")
	if err != nil {
		log.Fatalf("Failed to initialize storage: %v", err)
	}

	// Create proxy server
	proxyServer := proxy.NewServer(stor)

	// Setup HTTP server
	router := proxyServer.SetupRouter()

	httpServer := &http.Server{
		Addr:         ":5000",
		Handler:      router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	// Start HTTP server in goroutine
	go func() {
		log.Printf("Server listening on :5000")
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Handle graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("Shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		log.Fatalf("Server shutdown error: %v", err)
	}

	log.Println("Server shutdown complete")
}
