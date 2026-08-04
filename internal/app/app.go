package app

import (
	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/layout"
)

func Initialize(l layout.Layout) error {
	if err := l.Ensure(); err != nil {
		return err
	}
	return config.Initialize(l.ConfigPath())
}
