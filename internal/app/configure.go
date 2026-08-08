package app

import (
	"fmt"
	"sort"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/scheduler"
	"github.com/hypernewbie/vci/internal/store"
)

func load(l model.Layout) (config.Config, error) { return config.Load(l.ConfigPath()) }

// RequireCoordinatorRole returns an error when the configured root does
// not declare orchestrator = "self". Used by setup mutation paths so a
// client root cannot drift the coordinator's authoritative state.
func RequireCoordinatorRole(l model.Layout) error {
	cfg, err := config.Load(l.ConfigPath())
	if err != nil {
		return err
	}
	if cfg.Orchestrator != config.OrchestratorSelf {
		return fmt.Errorf("client root: orchestrator = %q", cfg.Orchestrator)
	}
	return nil
}

func AddMachine(l model.Layout, name string, machine config.Machine) error {
	if !model.ValidName(name) {
		return fmt.Errorf("invalid machine name %q", name)
	}
	if machine.MaxConcurrent < 0 {
		return fmt.Errorf("machine %q has negative max_concurrent", name)
	}
	if err := config.ValidateMachineHost(machine.Host); err != nil {
		return err
	}
	return config.Mutate(l.ConfigPath(), func(cfg *config.Config) error {
		if _, exists := cfg.Machines[name]; exists {
			return fmt.Errorf("machine %q already exists", name)
		}
		cfg.Machines[name] = machine
		return nil
	})
}

func RemoveMachine(l model.Layout, name string) error {
	return config.Mutate(l.ConfigPath(), func(cfg *config.Config) error {
		if _, exists := cfg.Machines[name]; !exists {
			return fmt.Errorf("machine %q does not exist", name)
		}
		for project, definition := range cfg.Projects {
			for _, attached := range definition.Machines {
				if attached == name {
					return fmt.Errorf("machine %q is attached to project %q", name, project)
				}
			}
		}
		delete(cfg.Machines, name)
		return nil
	})
}

func AddProject(l model.Layout, name string, project config.Project) error {
	if !model.ValidName(name) {
		return fmt.Errorf("invalid project name %q", name)
	}
	return config.Mutate(l.ConfigPath(), func(cfg *config.Config) error {
		if _, exists := cfg.Projects[name]; exists {
			return fmt.Errorf("project %q already exists", name)
		}
		cfg.Projects[name] = project
		return nil
	})
}

// SetHostedFallback sets hosted fallback on a coordinator project.
// It validates URL and commit before writing config.
// Client roots cannot call it.
func SetHostedFallback(l model.Layout, project, url, commit string) error {
	if !model.ValidName(project) {
		return fmt.Errorf("invalid project name %q", project)
	}
	if _, err := (config.HostedFallback{URL: url, Commit: commit}).Validate(); err != nil {
		return err
	}
	return config.Mutate(l.ConfigPath(), func(cfg *config.Config) error {
		if cfg.Orchestrator != config.OrchestratorSelf {
			return fmt.Errorf("client root: orchestrator = %q", cfg.Orchestrator)
		}
		proj, ok := cfg.Projects[project]
		if !ok {
			return fmt.Errorf("project %q does not exist", project)
		}
		proj.HostedFallback = config.HostedFallback{URL: url, Commit: commit}
		cfg.Projects[project] = proj
		return nil
	})
}

// ClearHostedFallback removes hosted fallback from a project.
// Client roots cannot call it.
// Error if the project is missing.
func ClearHostedFallback(l model.Layout, project string) error {
	if !model.ValidName(project) {
		return fmt.Errorf("invalid project name %q", project)
	}
	return config.Mutate(l.ConfigPath(), func(cfg *config.Config) error {
		if cfg.Orchestrator != config.OrchestratorSelf {
			return fmt.Errorf("client root: orchestrator = %q", cfg.Orchestrator)
		}
		proj, ok := cfg.Projects[project]
		if !ok {
			return fmt.Errorf("project %q does not exist", project)
		}
		proj.HostedFallback = config.HostedFallback{}
		cfg.Projects[project] = proj
		return nil
	})
}

type MachineView struct {
	Name      string         `json:"name"`
	Machine   config.Machine `json:"machine"`
	Capacity  int            `json:"capacity"`
	Active    int            `json:"active"`
	Available int            `json:"available"`
}
type ProjectView struct {
	Name    string         `json:"name"`
	Project config.Project `json:"project"`
}
type Inventory struct {
	Machines []MachineView `json:"machines"`
	Projects []ProjectView `json:"projects"`
}

func ReadInventory(l model.Layout) (Inventory, error) {
	cfg, err := load(l)
	if err != nil {
		return Inventory{}, err
	}
	machineNames := make([]string, 0, len(cfg.Machines))
	for name := range cfg.Machines {
		machineNames = append(machineNames, name)
	}
	sort.Strings(machineNames)
	projectNames := make([]string, 0, len(cfg.Projects))
	for name := range cfg.Projects {
		projectNames = append(projectNames, name)
	}
	sort.Strings(projectNames)
	runStore := store.Store{Layout: l}
	// A scheduler inspection failure is a state failure. Synthesizing
	// "available == capacity" on failure would falsely report free
	// slots and hide the operator's true inventory.
	statuses, statusErr := scheduler.Status(l, runStore, cfg)
	if statusErr != nil {
		return Inventory{}, statusErr
	}
	statusByMachine := map[string]scheduler.MachineStatus{}
	for _, s := range statuses {
		statusByMachine[s.Machine] = s
	}
	out := Inventory{Machines: make([]MachineView, 0, len(machineNames)), Projects: make([]ProjectView, 0, len(projectNames))}
	for _, name := range machineNames {
		capacity := config.EffectiveCapacity(cfg.Machines[name])
		view := MachineView{Name: name, Machine: cfg.Machines[name], Capacity: capacity}
		if s, ok := statusByMachine[name]; ok {
			view.Active = s.Active
			view.Available = s.Available
		}
		out.Machines = append(out.Machines, view)
	}
	for _, name := range projectNames {
		out.Projects = append(out.Projects, ProjectView{Name: name, Project: cfg.Projects[name]})
	}
	return out, nil
}

func Initialize(l model.Layout) error {
	if err := l.Ensure(); err != nil {
		return err
	}
	return config.Initialize(l.ConfigPath())
}
