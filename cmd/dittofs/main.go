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
	"github.com/marmos91/dittofs/internal/metadata/persistence"
	nfsServer "github.com/marmos91/dittofs/internal/server"
)

func createInitialStructure(repo *persistence.MemoryRepository, contentRepo *content.FSContentRepository, rootHandle metadata.FileHandle) error {
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

	imagesHandle, err := repo.AddFileToDirectory(rootHandle, "images", imagesAttr)
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

		if _, err := repo.AddFileToDirectory(imagesHandle, img.name, fileAttr); err != nil {
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

		if _, err := repo.AddFileToDirectory(rootHandle, txt.name, fileAttr); err != nil {
			return fmt.Errorf("failed to create %s: %w", txt.name, err)
		}
	}

	return nil
}

func main() {
	port := flag.String("port", "2049", "Port to listen on")
	logLevel := flag.String("log-level", "INFO", "Log level (DEBUG, INFO, WARN, ERROR)")
	contentPath := flag.String("content-path", "/tmp/dittofs-content", "Path to store file content")
	dumpRestricted := flag.Bool("dump-restricted", false, "Restrict DUMP to localhost only")
	flag.Parse()

	// Configure logger
	logger.SetLevel(*logLevel)

	fmt.Println("DittoFS - Dynamic NFS Server")
	logger.Info("Log level set to: %s", *logLevel)
	logger.Info("Content storage path: %s", *contentPath)

	// Create content repository
	contentRepo, err := content.NewFSContentRepository(*contentPath)
	if err != nil {
		log.Fatalf("Failed to create content repository: %v", err)
	}

	metadataRepo := persistence.NewMemoryRepository()

	// Configure server-wide settings
	serverConfig := metadata.ServerConfig{}
	if *dumpRestricted {
		// Restrict DUMP to localhost only
		serverConfig.DumpAllowedClients = []string{"127.0.0.1", "::1"}
		logger.Info("DUMP access restricted to localhost")
	} else {
		logger.Info("DUMP access unrestricted (default)")
	}

	if err := metadataRepo.SetServerConfig(serverConfig); err != nil {
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

	if err := metadataRepo.AddExport("/export", metadata.ExportOptions{
		ReadOnly:  false,
		Async:     true,
		AllSquash: true,
		AnonUID:   65534, // nobody
		AnonGID:   65534, // nogroup
	}, rootAttr); err != nil {
		log.Fatalf("Failed to add export: %v", err)
	}
	logger.Info("Export added: /export")

	metadataRepo.AddExport("/nolocalhost", metadata.ExportOptions{
		ReadOnly:           false,
		Async:              true,
		AllSquash:          true,
		AnonUID:            65534, // nobody
		AnonGID:            65534, // nogroup
		AllowedClients:     []string{"192.168.1.0/24"},
		DeniedClients:      []string{"192.168.1.50", "::1"},
		RequireAuth:        false,
		AllowedAuthFlavors: []uint32{0, 1}, // AUTH_NULL, AUTH_UNIX
	}, rootAttr)

	logger.Info("Export added: /nolocalhost")

	// Get root handle
	rootHandle, err := metadataRepo.GetRootHandle("/export")
	if err != nil {
		log.Fatalf("Failed to get root handle: %v", err)
	}

	// Create initial file structure
	if err := createInitialStructure(metadataRepo, contentRepo, rootHandle); err != nil {
		log.Fatalf("Failed to create initial structure: %v", err)
	}

	srv := nfsServer.New(*port, metadataRepo, contentRepo)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		if err := srv.Serve(ctx); err != nil {
			logger.Error("Server error: %v", err)
			os.Exit(1)
		}
	}()

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	logger.Info("Server is running. Press Ctrl+C to stop.")
	<-sigChan

	logger.Info("Shutting down server...")
}
