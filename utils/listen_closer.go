package utils

import (
	"github.com/corpetty/ortelius/cfg"
	"github.com/corpetty/ortelius/services"
)

// ListenCloser listens for messages until it's asked to close
type ListenCloser interface {
	Listen() error
	Close() error
}

type ListenCloserFactory func(*services.Control, cfg.Config, int, int) ListenCloser
