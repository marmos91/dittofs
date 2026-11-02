package vfs

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/vfs"
	"github.com/marmos91/dittofs/pkg/vfs/vfstest/suite"
)

func TestConformance(t *testing.T) {
	suite.Run(t, func(t *testing.T) vfs.Service { return newFilesystem(t) })
}
