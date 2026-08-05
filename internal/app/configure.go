package app

import (
	"fmt"
	"sort"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/layout"
)

func load(l layout.Layout) (config.Config, error) { return config.Load(l.ConfigPath()) }

// RequireCoordinatorRole returns an error when the configured root does
// not declare orchestrator = "self". Used by setup mutation paths so a
// client root cannot drift the coordinator's authoritative state.
func RequireCoordinatorRole(l layout.Layout) error {
	cfg, err := config.Load(l.ConfigPath())
	if err != nil {
		return err
	}
	if cfg.Orchestrator != config.OrchestratorSelf {
		return fmt.Errorf("client root: orchestrator = %q", cfg.Orchestrator)
	}
	return nil
}

func AddMachine(l layout.Layout, name string, machine config.Machine) error {
	if !layout.ValidName(name) {
		return fmt.Errorf("invalid machine name %q", name)
	}
	return config.Mutate(l.ConfigPath(), func(cfg *config.Config) error {
		if _, exists := cfg.Machines[name]; exists {
			return fmt.Errorf("machine %q already exists", name)
		}
		cfg.Machines[name] = machine
		return nil
	})
}

func RemoveMachine(l layout.Layout, name string) error {
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

func AddProject(l layout.Layout, name string, project config.Project) error {
	if !layout.ValidName(name) {
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

func UpdateProject(l layout.Layout, name string, project config.Project) error {
	if !layout.ValidName(name) {
		return fmt.Errorf("invalid project name %q", name)
	}
	return config.Mutate(l.ConfigPath(), func(cfg *config.Config) error {
		if _, exists := cfg.Projects[name]; !exists {
			return fmt.Errorf("project %q does not exist", name)
		}
		cfg.Projects[name] = project
		return nil
	})
}

func RemoveProject(l layout.Layout, name string) error {
	return config.Mutate(l.ConfigPath(), func(cfg *config.Config) error {
		if _, exists := cfg.Projects[name]; !exists {
			return fmt.Errorf("project %q does not exist", name)
		}
		delete(cfg.Projects, name)
		return nil
	})
}

type MachineView struct {
	Name    string         `json:"name"`
	Machine config.Machine `json:"machine"`
}
type ProjectView struct {
	Name    string         `json:"name"`
	Project config.Project `json:"project"`
}
type Inventory struct {
	Machines []MachineView `json:"machines"`
	Projects []ProjectView `json:"projects"`
}

func ReadInventory(l layout.Layout) (Inventory, error) {
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
	out := Inventory{Machines: make([]MachineView, 0, len(machineNames)), Projects: make([]ProjectView, 0, len(projectNames))}
	for _, name := range machineNames {
		out.Machines = append(out.Machines, MachineView{Name: name, Machine: cfg.Machines[name]})
	}
	for _, name := range projectNames {
		out.Projects = append(out.Projects, ProjectView{Name: name, Project: cfg.Projects[name]})
	}
	return out, nil
}
