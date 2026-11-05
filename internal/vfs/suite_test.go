package vfs

import (
	"testing"

	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest/suite"
)

func TestConformance(t *testing.T) {
	suite.Run(t, func(t *testing.T) vfs.Service { return newFilesystem(t) })
}
