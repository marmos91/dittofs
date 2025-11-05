package server

import (
	"context"
	"fmt"
	"net"

	"github.com/marmos91/dittofs/internal/content"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/metadata"
	"github.com/marmos91/dittofs/internal/protocol/mount"
	"github.com/marmos91/dittofs/internal/protocol/nfs"
)

type NFSServer struct {
	port         string
	listener     net.Listener
	nfsHandler   NFSHandler
	mountHandler MountHandler
	repository   metadata.Repository
	content      content.Repository
}

func New(port string, repository metadata.Repository, content content.Repository) *NFSServer {
	return &NFSServer{
		port:         port,
		nfsHandler:   &nfs.DefaultNFSHandler{},
		mountHandler: &mount.DefaultMountHandler{},
		repository:   repository,
		content:      content,
	}
}

// RegisterNFSHandler registers a custom NFS handler
func (s *NFSServer) RegisterNFSHandler(handler NFSHandler) {
	s.nfsHandler = handler
}

// RegisterMountHandler registers a custom Mount handler
func (s *NFSServer) RegisterMountHandler(handler MountHandler) {
	s.mountHandler = handler
}

// GetRepository returns the server's repository instance
func (s *NFSServer) GetRepository() metadata.Repository {
	return s.repository
}

func (s *NFSServer) Serve(ctx context.Context) error {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%s", s.port))
	if err != nil {
		return fmt.Errorf("failed to start listener: %w", err)
	}

	s.listener = listener
	logger.Debug("NFS server started on port %s", s.port)

	go func() {
		<-ctx.Done()
		s.listener.Close()
	}()

	for {
		tcpConn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				logger.Debug("Error accepting connection: %v", err)
				continue

			}
		}

		conn := s.newConn(tcpConn)
		go conn.serve(ctx)
	}
}

func (s *NFSServer) newConn(tcpConn net.Conn) *conn {
	c := &conn{
		server: s,
		conn:   tcpConn,
	}

	return c
}

func (s *NFSServer) Stop() error {
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}
