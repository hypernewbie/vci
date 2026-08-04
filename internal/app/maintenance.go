package app

import (
	"time"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/reaper"
	"github.com/hypernewbie/vci/internal/retention"
)

type MaintenanceReport struct {
	Reaped    reaper.Report    `json:"reaped"`
	Retention retention.Report `json:"retention"`
}

func Maintain(l layout.Layout) (MaintenanceReport, error) {
	cfg, err := config.Load(l.ConfigPath())
	if err != nil {
		return MaintenanceReport{}, err
	}
	reaped, err := reaper.Run(l, time.Now().UTC())
	if err != nil {
		return MaintenanceReport{}, err
	}
	retained, err := retention.Enforce(l, cfg.Retention)
	if err != nil {
		return MaintenanceReport{}, err
	}
	return MaintenanceReport{Reaped: reaped, Retention: retained}, nil
}
