// Copyright (c) 2026 TRV Enterprises LLC
// Licensed under the PolyForm Noncommercial License 1.0.0
// See LICENSE file for details.

// Package main is the entry point for the tsstore CLI.
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

	"github.com/gin-gonic/gin"
	"github.com/tviviano/ts-store/internal/apikey"
	"github.com/tviviano/ts-store/internal/config"
	"github.com/tviviano/ts-store/internal/handlers"
	"github.com/tviviano/ts-store/internal/middleware"
	"github.com/tviviano/ts-store/internal/service"
)

const (
	defaultConfigPath = "config.json"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	command := os.Args[1]

	switch command {
	case "serve":
		runServer()
	case "create":
		runCreateCommand(os.Args[2:])
	case "key":
		if len(os.Args) < 3 {
			printKeyUsage()
			os.Exit(1)
		}
		runKeyCommand(os.Args[2:])
	case "help", "-h", "--help":
		printUsage()
	case "version", "-v", "--version":
		fmt.Println("tsstore v0.1.0")
	default:
		fmt.Printf("Unknown command: %s\n", command)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`tsstore - Time Series Store Server

Usage:
  tsstore <command> [arguments]

Commands:
  serve     Start the API server
  create    Create a new store
  key       Manage API keys (requires device access)
  help      Show this help message
  version   Show version

Use "tsstore <command> -h" for more information about a command.`)
}

func printCreateUsage() {
	fmt.Println(`tsstore create - Create a new store

Usage:
  tsstore create <store-name> [options]

Options:
  --blocks <n>       Number of primary blocks (default: 1024)
  --data-size <n>    Data block size in bytes, must be power of 2 (default: 4096)
  --index-size <n>   Index block size in bytes, must be power of 2 (default: 4096)
  --path <dir>       Base directory for stores (default: ./data or TSSTORE_DATA_PATH)

Examples:
  tsstore create my-store
  tsstore create sensors --blocks 10000 --data-size 8192
  tsstore create logs --path /var/tsstore`)
}

func printKeyUsage() {
	fmt.Println(`tsstore key - Manage API keys

Usage:
  tsstore key <subcommand> [arguments]

Subcommands:
  regenerate <store-name>  Regenerate API key for a store (revokes all existing keys)
  list <store-name>        List API keys for a store (shows IDs, not keys)
  revoke <store-name> <key-id>  Revoke a specific key by ID

Examples:
  tsstore key regenerate my-store
  tsstore key list my-store
  tsstore key revoke my-store a1b2c3d4`)
}

func runServer() {
	// Load configuration
	configPath := defaultConfigPath
	if envPath := os.Getenv("TSSTORE_CONFIG"); envPath != "" {
		configPath = envPath
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	cfg.LoadFromEnv()

	// Ensure data directory exists
	if err := os.MkdirAll(cfg.Store.BasePath, 0755); err != nil {
		log.Fatalf("Failed to create data directory: %v", err)
	}

	// Initialize components
	keyManager := apikey.NewManager(cfg.Store.BasePath)
	storeService := service.NewStoreService(cfg, keyManager)

	// Setup Gin
	if cfg.Server.Mode == "release" {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.New()
	router.Use(gin.Recovery())
	router.Use(middleware.CORS())
	router.Use(middleware.RequestLogger())

	// Health check (no auth required)
	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status": "ok",
			"time":   time.Now().UTC().Format(time.RFC3339),
		})
	})

	// Initialize handlers
	storeHandler := handlers.NewStoreHandler(storeService)
	dataHandler := handlers.NewDataHandler(storeService)
	objectHandler := handlers.NewObjectHandler(storeService)
	jsonHandler := handlers.NewJSONHandler(storeService)

	// API routes
	api := router.Group("/api")
	{
		// Store management (no auth for create, list)
		stores := api.Group("/stores")
		{
			stores.POST("", storeHandler.Create)       // Create new store (returns API key)
			stores.GET("", storeHandler.List)          // List open stores
		}

		// Store-specific operations (require auth)
		storeRoutes := stores.Group("/:store")
		storeRoutes.Use(middleware.Auth(keyManager))
		{
			storeRoutes.DELETE("", storeHandler.Delete)
			storeRoutes.GET("/stats", storeHandler.Stats)

			// Low-level data operations
			data := storeRoutes.Group("/data")
			{
				data.POST("", dataHandler.Insert)
				data.GET("/time/:timestamp", dataHandler.GetByTime)
				data.GET("/range", dataHandler.GetRange)
				data.GET("/oldest", dataHandler.GetOldest)
				data.GET("/newest", dataHandler.GetNewest)
			}

			// Object operations (base64 encoded data)
			objects := storeRoutes.Group("/objects")
			{
				objects.POST("", objectHandler.Put)
				objects.GET("/time/:timestamp", objectHandler.GetByTime)
				objects.DELETE("/time/:timestamp", objectHandler.DeleteByTime)
				objects.GET("/oldest", objectHandler.ListOldest)
				objects.GET("/newest", objectHandler.ListNewest)
				objects.GET("/range", objectHandler.ListRange)
			}

			// JSON object operations (no base64 encoding needed)
			jsonRoutes := storeRoutes.Group("/json")
			{
				jsonRoutes.POST("", jsonHandler.Put)
				jsonRoutes.GET("/time/:timestamp", jsonHandler.GetByTime)
				jsonRoutes.GET("/oldest", jsonHandler.ListOldest)
				jsonRoutes.GET("/newest", jsonHandler.ListNewest)  // Supports ?since=2h
				jsonRoutes.GET("/range", jsonHandler.ListRange)    // Supports ?since=2h or ?start_time=X&end_time=Y
			}
		}
	}

	// Create server
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      router,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start server in goroutine
	go func() {
		log.Printf("Starting tsstore server on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("Server shutdown error: %v", err)
	}

	// Close all stores
	if err := storeService.CloseAll(); err != nil {
		log.Printf("Error closing stores: %v", err)
	}

	log.Println("Server stopped")
}

func runCreateCommand(args []string) {
	if len(args) < 1 || args[0] == "-h" || args[0] == "--help" {
		printCreateUsage()
		if len(args) < 1 {
			os.Exit(1)
		}
		return
	}

	storeName := args[0]

	// Parse options
	numBlocks := uint32(1024)
	dataBlockSize := uint32(4096)
	indexBlockSize := uint32(4096)
	basePath := ""

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--blocks":
			if i+1 < len(args) {
				i++
				var n int
				fmt.Sscanf(args[i], "%d", &n)
				if n > 0 {
					numBlocks = uint32(n)
				}
			}
		case "--data-size":
			if i+1 < len(args) {
				i++
				var n int
				fmt.Sscanf(args[i], "%d", &n)
				if n > 0 {
					dataBlockSize = uint32(n)
				}
			}
		case "--index-size":
			if i+1 < len(args) {
				i++
				var n int
				fmt.Sscanf(args[i], "%d", &n)
				if n > 0 {
					indexBlockSize = uint32(n)
				}
			}
		case "--path":
			if i+1 < len(args) {
				i++
				basePath = args[i]
			}
		}
	}

	// Load config for defaults
	configPath := defaultConfigPath
	if envPath := os.Getenv("TSSTORE_CONFIG"); envPath != "" {
		configPath = envPath
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	cfg.LoadFromEnv()

	// Override base path if specified
	if basePath != "" {
		cfg.Store.BasePath = basePath
	}

	// Ensure base directory exists
	if err := os.MkdirAll(cfg.Store.BasePath, 0755); err != nil {
		log.Fatalf("Failed to create data directory: %v", err)
	}

	// Create the store
	keyManager := apikey.NewManager(cfg.Store.BasePath)
	storeService := service.NewStoreService(cfg, keyManager)

	req := &service.CreateStoreRequest{
		Name:           storeName,
		NumBlocks:      numBlocks,
		DataBlockSize:  dataBlockSize,
		IndexBlockSize: indexBlockSize,
	}

	resp, err := storeService.Create(req)
	if err != nil {
		log.Fatalf("Failed to create store: %v", err)
	}

	// Close the store
	storeService.CloseAll()

	fmt.Println("=== STORE CREATED ===")
	fmt.Printf("Name:        %s\n", resp.Name)
	fmt.Printf("Path:        %s/%s\n", cfg.Store.BasePath, resp.Name)
	fmt.Printf("Blocks:      %d\n", numBlocks)
	fmt.Printf("Data Size:   %d bytes\n", dataBlockSize)
	fmt.Printf("Index Size:  %d bytes\n", indexBlockSize)
	fmt.Println("")
	fmt.Printf("Key ID:      %s\n", resp.KeyID)
	fmt.Printf("API Key:     %s\n", resp.APIKey)
	fmt.Println("")
	fmt.Println("WARNING: The API key is shown only once. Save it securely!")
}

func runKeyCommand(args []string) {
	if len(args) < 1 {
		printKeyUsage()
		os.Exit(1)
	}

	// Load config to get base path
	configPath := defaultConfigPath
	if envPath := os.Getenv("TSSTORE_CONFIG"); envPath != "" {
		configPath = envPath
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	cfg.LoadFromEnv()

	keyManager := apikey.NewManager(cfg.Store.BasePath)

	subCommand := args[0]
	switch subCommand {
	case "regenerate":
		if len(args) < 2 {
			fmt.Println("Error: store name required")
			printKeyUsage()
			os.Exit(1)
		}
		storeName := args[1]

		// Regenerate key
		newKey, entry, err := keyManager.Regenerate(storeName, "Regenerated via CLI")
		if err != nil {
			log.Fatalf("Failed to regenerate key: %v", err)
		}

		fmt.Println("=== NEW API KEY ===")
		fmt.Printf("Store:   %s\n", storeName)
		fmt.Printf("Key ID:  %s\n", entry.ID)
		fmt.Printf("API Key: %s\n", newKey)
		fmt.Println("")
		fmt.Println("WARNING: This key is shown only once. Save it securely!")
		fmt.Println("All previous keys have been revoked.")

	case "list":
		if len(args) < 2 {
			fmt.Println("Error: store name required")
			printKeyUsage()
			os.Exit(1)
		}
		storeName := args[1]

		keys, err := keyManager.List(storeName)
		if err != nil {
			log.Fatalf("Failed to list keys: %v", err)
		}

		if len(keys) == 0 {
			fmt.Printf("No API keys found for store '%s'\n", storeName)
			return
		}

		fmt.Printf("API keys for store '%s':\n", storeName)
		fmt.Println("ID        Created                    Note")
		fmt.Println("--------  -------------------------  ----")
		for _, k := range keys {
			fmt.Printf("%-8s  %-25s  %s\n",
				k.ID,
				k.CreatedAt.Format("2006-01-02 15:04:05 MST"),
				k.Note)
		}

	case "revoke":
		if len(args) < 3 {
			fmt.Println("Error: store name and key ID required")
			printKeyUsage()
			os.Exit(1)
		}
		storeName := args[1]
		keyID := args[2]

		if err := keyManager.Revoke(storeName, keyID); err != nil {
			log.Fatalf("Failed to revoke key: %v", err)
		}

		fmt.Printf("Key '%s' revoked for store '%s'\n", keyID, storeName)

	default:
		fmt.Printf("Unknown key subcommand: %s\n", subCommand)
		printKeyUsage()
		os.Exit(1)
	}
}
