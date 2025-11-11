package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/marmos91/dittofs/internal/content"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
	"github.com/marmos91/dittofs/internal/metadata/persistence/memory"
	nfsServer "github.com/marmos91/dittofs/internal/server"
)

func createInitialStructure(ctx context.Context, repo *memory.MemoryRepository, contentRepo *content.FSContentRepository, rootHandle metadata.FileHandle) error {
	now := time.Now()

	// Create "images" directory
	imagesAttr := &metadata.FileAttr{
		Type:      metadata.FileTypeDirectory,
		Mode:      0755,
		UID:       501,
		GID:       20,
		Size:      4096,
		Atime:     now,
		Mtime:     now,
		Ctime:     now,
		ContentID: "", // Directories don't have content
	}

	imagesHandle, err := repo.AddFileToDirectory(ctx, rootHandle, "images", imagesAttr)
	if err != nil {
		return fmt.Errorf("failed to create images directory: %w", err)
	}

	// Create image files inside images directory with actual content
	imageFiles := []struct {
		name    string
		content string
	}{
		{"background1.png", "PNG image content for background1"},
		{"background2.jpg", "JPEG image content for background2"},
		{"wallpaper.png", "PNG image content for wallpaper"},
	}

	for _, img := range imageFiles {
		contentID := content.ContentID(fmt.Sprintf("img-%s", img.name))

		// Write actual content to the content repository
		if err := contentRepo.WriteContent(contentID, []byte(img.content)); err != nil {
			return fmt.Errorf("failed to write content for %s: %w", img.name, err)
		}

		fileAttr := &metadata.FileAttr{
			Type:      metadata.FileTypeRegular,
			Mode:      0644,
			UID:       501,
			GID:       20,
			Size:      uint64(len(img.content)),
			Atime:     now,
			Mtime:     now,
			Ctime:     now,
			ContentID: contentID,
		}

		if _, err := repo.AddFileToDirectory(ctx, imagesHandle, img.name, fileAttr); err != nil {
			return fmt.Errorf("failed to create %s: %w", img.name, err)
		}
	}

	// Create text files in root
	textFiles := []struct {
		name    string
		content string
	}{
		{"readme.txt", "This is a README file.\nWelcome to dittofs!\n"},
		{"notes.txt", "Some notes about this NFS server.\nIt's pretty cool!\n"},
	}

	for _, txt := range textFiles {
		contentID := content.ContentID(fmt.Sprintf("txt-%s", txt.name))

		// Write actual content to the content repository
		if err := contentRepo.WriteContent(contentID, []byte(txt.content)); err != nil {
			return fmt.Errorf("failed to write content for %s: %w", txt.name, err)
		}

		fileAttr := &metadata.FileAttr{
			Type:      metadata.FileTypeRegular,
			Mode:      0644,
			UID:       501,
			GID:       20,
			Size:      uint64(len(txt.content)),
			Atime:     now,
			Mtime:     now,
			Ctime:     now,
			ContentID: contentID,
		}

		if _, err := repo.AddFileToDirectory(ctx, rootHandle, txt.name, fileAttr); err != nil {
			return fmt.Errorf("failed to create %s: %w", txt.name, err)
		}
	}

	return nil
}

func main() {
	// Server configuration flags
	port := flag.String("port", "2049", "Port to listen on")
	logLevel := flag.String("log-level", "INFO", "Log level (DEBUG, INFO, WARN, ERROR)")
	contentPath := flag.String("content-path", "/tmp/dittofs-content", "Path to store file content")

	// Connection limit flags
	maxConnections := flag.Int("max-connections", 0, "Maximum concurrent connections (0 = unlimited)")
	readTimeout := flag.Duration("read-timeout", 30*time.Second, "Read timeout for RPC requests")
	writeTimeout := flag.Duration("write-timeout", 30*time.Second, "Write timeout for RPC responses")
	idleTimeout := flag.Duration("idle-timeout", 5*time.Minute, "Idle timeout between requests")
	shutdownTimeout := flag.Duration("shutdown-timeout", 30*time.Second, "Graceful shutdown timeout")

	// Repository configuration flags
	dumpRestricted := flag.Bool("dump-restricted", false, "Restrict DUMP to localhost only")

	// Metrics flags
	metricsInterval := flag.Duration("metrics-interval", 5*time.Minute, "Interval for logging metrics (0 to disable)")

	flag.Parse()

	// Configure logger
	logger.SetLevel(*logLevel)

	// Create cancellable context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fmt.Println("DittoFS - Dynamic NFS Server")
	logger.Info("Log level set to: %s", *logLevel)
	logger.Info("Content storage path: %s", *contentPath)

	// Create content repository
	contentRepo, err := content.NewFSContentRepository(*contentPath)
	if err != nil {
		log.Fatalf("Failed to create content repository: %v", err)
	}

	metadataRepo := memory.NewMemoryRepository()

	// Configure metadata repository settings
	repoConfig := metadata.ServerConfig{}
	if *dumpRestricted {
		// Restrict DUMP to localhost only
		repoConfig.DumpAllowedClients = []string{"127.0.0.1", "::1"}
		logger.Info("DUMP access restricted to localhost")
	} else {
		logger.Info("DUMP access unrestricted (default)")
	}

	if err := metadataRepo.SetServerConfig(ctx, repoConfig); err != nil {
		log.Fatalf("Failed to set server config: %v", err)
	}

	// Create root directory attributes
	now := time.Now()
	rootAttr := &metadata.FileAttr{
		Type:      metadata.FileTypeDirectory,
		Mode:      0755,
		UID:       501,
		GID:       20,
		Size:      4096,
		Atime:     now,
		Mtime:     now,
		Ctime:     now,
		ContentID: "", // Root directory has no content
	}

	anonUID := uint32(metadata.DefaultAnonUID)
	anonGID := uint32(metadata.DefaultAnonGID)

	// Add primary export
	if err := metadataRepo.AddExport(ctx, "/export", metadata.ExportOptions{
		ReadOnly:  false,
		Async:     true,
		AllSquash: true,
		AnonUID:   &anonUID, // nobody
		AnonGID:   &anonGID, // nogroup
	}, rootAttr); err != nil {
		log.Fatalf("Failed to add export: %v", err)
	}
	logger.Info("Export added: /export (read-write, all_squash)")

	// Add restricted export example
	if err := metadataRepo.AddExport(ctx, "/nolocalhost", metadata.ExportOptions{
		ReadOnly:           false,
		Async:              true,
		AllSquash:          true,
		AnonUID:            &anonUID, // nobody
		AnonGID:            &anonGID, // nogroup
		AllowedClients:     []string{"192.168.1.0/24"},
		DeniedClients:      []string{"192.168.1.50", "::1"},
		RequireAuth:        false,
		AllowedAuthFlavors: []uint32{0, 1}, // AUTH_NULL, AUTH_UNIX
	}, rootAttr); err != nil {
		log.Fatalf("Failed to add restricted export: %v", err)
	}
	logger.Info("Export added: /nolocalhost (network restricted)")

	// Get root handle for initial structure creation
	rootHandle, err := metadataRepo.GetRootHandle(ctx, "/export")
	if err != nil {
		log.Fatalf("Failed to get root handle: %v", err)
	}

	// Create initial file structure
	if err := createInitialStructure(ctx, metadataRepo, contentRepo, rootHandle); err != nil {
		log.Fatalf("Failed to create initial structure: %v", err)
	}
	logger.Info("Initial file structure created")

	// Update server configuration section:
	serverConfig := nfsServer.ServerConfig{
		Port:               *port,
		MaxConnections:     *maxConnections,
		ReadTimeout:        *readTimeout,
		WriteTimeout:       *writeTimeout,
		IdleTimeout:        *idleTimeout,
		ShutdownTimeout:    *shutdownTimeout,
		MetricsLogInterval: *metricsInterval,
	}

	// Log server configuration
	logger.Info("Server configuration:")
	logger.Info("  Port: %s", serverConfig.Port)
	if serverConfig.MaxConnections > 0 {
		logger.Info("  Max connections: %d", serverConfig.MaxConnections)
	} else {
		logger.Info("  Max connections: unlimited")
	}
	logger.Info("  Read timeout: %v", serverConfig.ReadTimeout)
	logger.Info("  Write timeout: %v", serverConfig.WriteTimeout)
	logger.Info("  Idle timeout: %v", serverConfig.IdleTimeout)
	logger.Info("  Shutdown timeout: %v", serverConfig.ShutdownTimeout)
	logger.Info("  Metrics interval: %v", serverConfig.MetricsLogInterval)

	if serverConfig.MetricsLogInterval == 0 {
		logger.Info("  (metrics logging disabled)")
	}

	// Create NFS server with configuration
	srv := nfsServer.New(serverConfig, metadataRepo, contentRepo)

	// Start server in background
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- srv.Serve(ctx)
	}()

	// Wait for interrupt signal or server error
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	logger.Info("Server is running on port %s. Press Ctrl+C to stop.", *port)

	select {
	case <-sigChan:
		logger.Info("Shutdown signal received, initiating graceful shutdown...")
		cancel() // Cancel context to initiate shutdown

		// Wait for server to shut down gracefully
		if err := <-serverDone; err != nil {
			logger.Error("Server shutdown error: %v", err)
			os.Exit(1)
		}
		logger.Info("Server stopped gracefully")

	case err := <-serverDone:
		if err != nil {
			logger.Error("Server error: %v", err)
			os.Exit(1)
		}
		logger.Info("Server stopped")
	}
}
