//go:build e2e

package framework

import (
	"context"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
)

// TestImageObtainable_RefusesAnUnpullableImage pins the property the emulator
// guards exist for: an image Docker cannot supply must be refused, not assumed
// available. The pre-fix CheckMinioAvailable returned true here without checking
// anything, which is how an unpullable minio image reached NewMinioHelper's
// t.Fatalf and reddened every E2E run.
func TestImageObtainable_RefusesAnUnpullableImage(t *testing.T) {
	provider, err := testcontainers.NewDockerProvider()
	if err != nil {
		t.Skipf("Docker unavailable: %v", err)
	}
	defer func() { _ = provider.Close() }()

	// A syntactically valid reference to a repository that does not exist. A
	// short parent deadline bounds the probe even when the failure is not a
	// permanent client error and the pull retries.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if imageObtainable(ctx, "dittofs-e2e-nonexistent/nope:does-not-exist") {
		t.Fatal("imageObtainable reported an unpullable image as obtainable")
	}
}

// TestImageObtainable_AcceptsALocalImage pins the other direction: an image the
// daemon already holds is reported obtainable, so the honest guard does not turn
// a usable emulator into a skip. It uses whatever the daemon has on hand rather
// than pulling, so it needs no registry access.
func TestImageObtainable_AcceptsALocalImage(t *testing.T) {
	provider, err := testcontainers.NewDockerProvider()
	if err != nil {
		t.Skipf("Docker unavailable: %v", err)
	}
	defer func() { _ = provider.Close() }()

	images, err := provider.ListImages(context.Background())
	if err != nil {
		t.Skipf("cannot list local images: %v", err)
	}
	if len(images) == 0 {
		t.Skip("no local Docker images to probe")
	}

	if !imageObtainable(context.Background(), images[0].Name) {
		t.Fatalf("imageObtainable(%q) = false for a locally present image", images[0].Name)
	}
}
