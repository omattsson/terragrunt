package configstack

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/gruntwork-io/terragrunt/internal/errors"
	"github.com/gruntwork-io/terragrunt/internal/experiment"
	"github.com/gruntwork-io/terragrunt/internal/report"
	"github.com/gruntwork-io/terragrunt/options"
	"github.com/gruntwork-io/terragrunt/pkg/log"
	"github.com/gruntwork-io/terragrunt/telemetry"
	"github.com/gruntwork-io/terragrunt/tf"
)

const (
	Waiting ModuleStatus = iota
	Running
	Finished
	channelSize = 1000 // Use a huge buffer to ensure senders are never blocked
)

const (
	NormalOrder DependencyOrder = iota
	ReverseOrder
	IgnoreOrder
)

// ModuleStatus represents the status of a module that we are
// trying to apply or destroy as part of the run --all apply or run --all destroy command
type ModuleStatus int

// DependencyOrder controls in what order dependencies should be enforced between modules.
type DependencyOrder int

// RunningModule represents a module we are trying to "run" (i.e. apply or destroy)
// as part of the run --all apply or run --all destroy command.
type RunningModule struct {
	Err            error
	Module         *TerraformModule
	Logger         log.Logger
	DependencyDone chan *RunningModule
	Dependencies   map[string]*RunningModule
	NotifyWhenDone []*RunningModule
	Status         ModuleStatus
	FlagExcluded   bool
}

// Create a new RunningModule struct for the given module. This will initialize all fields to reasonable defaults,
// except for the Dependencies and NotifyWhenDone, both of which will be empty. You should fill these using a
// function such as crossLinkDependencies.
func newRunningModule(module *TerraformModule) *RunningModule {
	return &RunningModule{
		Module:         module,
		Status:         Waiting,
		DependencyDone: make(chan *RunningModule, channelSize),
		Dependencies:   map[string]*RunningModule{},
		Logger:         module.Logger,
		NotifyWhenDone: []*RunningModule{},
		FlagExcluded:   module.FlagExcluded,
	}
}

// Run a module once all of its dependencies have finished executing.
func (module *RunningModule) runModuleWhenReady(ctx context.Context, opts *options.TerragruntOptions, r *report.Report, semaphore chan struct{}) {
	err := telemetry.TelemeterFromContext(ctx).Collect(ctx, "wait_for_module_ready", map[string]any{
		"path":             module.Module.Path,
		"terraformCommand": module.Module.TerragruntOptions.TerraformCommand,
	}, func(_ context.Context) error {
		return module.waitForDependencies(opts, r)
	})

	semaphore <- struct{}{} // Add one to the buffered channel. Will block if parallelism limit is met
	defer func() {
		<-semaphore // Remove one from the buffered channel
	}()

	if err == nil {
		err = telemetry.TelemeterFromContext(ctx).Collect(ctx, "run_module", map[string]any{
			"path":             module.Module.Path,
			"terraformCommand": module.Module.TerragruntOptions.TerraformCommand,
		}, func(ctx context.Context) error {
			return module.runNow(ctx, opts, r)
		})
	}

	module.moduleFinished(err, r, opts.Experiments.Evaluate(experiment.Report))
}

// Wait for all of this modules dependencies to finish executing. Return an error if any of those dependencies complete
// with an error. Return immediately if this module has no dependencies.
func (module *RunningModule) waitForDependencies(opts *options.TerragruntOptions, r *report.Report) error {
	module.Logger.Debugf("Module %s must wait for %d dependencies to finish", module.Module.Path, len(module.Dependencies))

	for len(module.Dependencies) > 0 {
		doneDependency := <-module.DependencyDone
		delete(module.Dependencies, doneDependency.Module.Path)

		if doneDependency.Err != nil {
			if module.Module.TerragruntOptions.IgnoreDependencyErrors {
				module.Logger.Errorf("Dependency %s of module %s just finished with an error. Module %s will have to return an error too. However, because of --queue-ignore-errors, module %s will run anyway.", doneDependency.Module.Path, module.Module.Path, module.Module.Path, module.Module.Path)
			} else {
				module.Logger.Errorf("Dependency %s of module %s just finished with an error. Module %s will have to return an error too.", doneDependency.Module.Path, module.Module.Path, module.Module.Path)

				if opts.Experiments.Evaluate(experiment.Report) {
					run, err := r.EnsureRun(module.Module.Path)
					if err != nil {
						module.Logger.Errorf("Error ensuring run for unit %s: %v", module.Module.Path, err)
						return err
					}

					if err := r.EndRun(
						run.Path,
						report.WithResult(report.ResultEarlyExit),
						report.WithReason(report.ReasonAncestorError),
						report.WithCauseAncestorExit(doneDependency.Module.Path),
					); err != nil {
						module.Logger.Errorf("Error ending run for unit %s: %v", module.Module.Path, err)
					}
				}

				return ProcessingModuleDependencyError{module.Module, doneDependency.Module, doneDependency.Err}
			}
		} else {
			module.Logger.Debugf("Dependency %s of module %s just finished successfully. Module %s must wait on %d more dependencies.", doneDependency.Module.Path, module.Module.Path, module.Module.Path, len(module.Dependencies))
		}
	}

	return nil
}

func (module *RunningModule) runTerragrunt(ctx context.Context, opts *options.TerragruntOptions, r *report.Report) error {
	module.Logger.Debugf("Running %s", module.Module.Path)

	opts.Writer = NewModuleWriter(opts.Writer)

	defer module.Module.FlushOutput() //nolint:errcheck

	if opts.Experiments.Evaluate(experiment.Report) {
		run, err := report.NewRun(module.Module.Path)
		if err != nil {
			return err
		}

		if err := r.AddRun(run); err != nil {
			return err
		}
	}

	return opts.RunTerragrunt(ctx, module.Logger, opts, r)
}

// Run a module right now by executing the RunTerragrunt command of its TerragruntOptions field.
func (module *RunningModule) runNow(ctx context.Context, rootOptions *options.TerragruntOptions, r *report.Report) error {
	module.Status = Running

	if module.Module.AssumeAlreadyApplied {
		module.Logger.Debugf("Assuming module %s has already been applied and skipping it", module.Module.Path)
		return nil
	} else {
		if err := module.runTerragrunt(ctx, module.Module.TerragruntOptions, r); err != nil {
			return err
		}

		// convert terragrunt output to json
		if module.Module.outputJSONFile(module.Logger, module.Module.TerragruntOptions) != "" {
			l, jsonOptions, err := module.Module.TerragruntOptions.CloneWithConfigPath(module.Logger, module.Module.TerragruntOptions.TerragruntConfigPath)
			if err != nil {
				return err
			}

			stdout := bytes.Buffer{}
			jsonOptions.ForwardTFStdout = true
			jsonOptions.JSONLogFormat = false
			jsonOptions.Writer = &stdout
			jsonOptions.TerraformCommand = tf.CommandNameShow
			jsonOptions.TerraformCliArgs = []string{tf.CommandNameShow, "-json", module.Module.planFile(l, rootOptions)}

			if err := jsonOptions.RunTerragrunt(ctx, l, jsonOptions, r); err != nil {
				return err
			}

			// save the json output to the file plan file
			outputFile := module.Module.outputJSONFile(l, rootOptions)
			jsonDir := filepath.Dir(outputFile)

			if err := os.MkdirAll(jsonDir, os.ModePerm); err != nil {
				return err
			}

			if err := os.WriteFile(outputFile, stdout.Bytes(), os.ModePerm); err != nil {
				return err
			}
		}

		return nil
	}
}

// Record that a module has finished executing and notify all of this module's dependencies
func (module *RunningModule) moduleFinished(moduleErr error, r *report.Report, reportExperiment bool) {
	if moduleErr == nil {
		module.Logger.Debugf("Module %s has finished successfully!", module.Module.Path)

		if reportExperiment {
			if err := r.EndRun(module.Module.Path); err != nil {
				// If the run is not found in the report, it likely means this module was an external dependency
				// that was excluded from the queue (e.g., with --queue-exclude-external).
				if !errors.Is(err, report.ErrRunNotFound) {
					module.Logger.Errorf("Error ending run for unit %s: %v", module.Module.Path, err)

					return
				}

				if module.Module.AssumeAlreadyApplied {
					run, err := report.NewRun(module.Module.Path)
					if err != nil {
						module.Logger.Errorf("Error creating run for unit %s: %v", module.Module.Path, err)
						return
					}

					if err := r.AddRun(run); err != nil {
						module.Logger.Errorf("Error adding run for unit %s: %v", module.Module.Path, err)
						return
					}

					if err := r.EndRun(
						run.Path,
						report.WithResult(report.ResultExcluded),
						report.WithReason(report.ReasonExcludeExternal),
					); err != nil {
						module.Logger.Errorf("Error ending run for unit %s: %v", module.Module.Path, err)
					}
				}
			}
		}
	} else {
		module.Logger.Errorf("Module %s has finished with an error", module.Module.Path)

		if reportExperiment {
			if err := r.EndRun(
				module.Module.Path,
				report.WithResult(report.ResultFailed),
				report.WithReason(report.ReasonRunError),
				report.WithCauseRunError(moduleErr.Error()),
			); err != nil {
				if errors.Is(err, report.ErrRunNotFound) {
					run, err := report.NewRun(module.Module.Path)
					if err != nil {
						module.Logger.Errorf("Error creating run for unit %s: %v", module.Module.Path, err)
						return
					}

					if err := r.AddRun(run); err != nil {
						module.Logger.Errorf("Error adding run for unit %s: %v", module.Module.Path, err)
						return
					}

					if err := r.EndRun(
						run.Path,
						report.WithResult(report.ResultFailed),
						report.WithReason(report.ReasonRunError),
						report.WithCauseRunError(moduleErr.Error()),
					); err != nil {
						module.Logger.Errorf("Error ending run for unit %s: %v", module.Module.Path, err)
					}
				} else {
					module.Logger.Errorf("Error ending run for unit %s: %v", module.Module.Path, err)
				}
			}
		}
	}

	module.Status = Finished
	module.Err = moduleErr

	for _, toNotify := range module.NotifyWhenDone {
		toNotify.DependencyDone <- module
	}
}

type RunningModules map[string]*RunningModule

func (modules RunningModules) toTerraformModuleGroups(maxDepth int) []TerraformModules {
	// Walk the graph in run order, capturing which groups will run at each iteration. In each iteration, this pops out
	// the modules that have no dependencies and captures that as a run group.
	groups := []TerraformModules{}

	for len(modules) > 0 && len(groups) < maxDepth {
		currentIterationDeploy := TerraformModules{}

		// next tracks which modules are being deferred to a later run.
		next := RunningModules{}
		// removeDep tracks which modules are run in the current iteration so that they need to be removed in the
		// dependency list for the next iteration. This is separately tracked from currentIterationDeploy for
		// convenience: this tracks the map key of the Dependencies attribute.
		var removeDep []string

		// Iterate the modules, looking for those that have no dependencies and select them for "running". In the
		// process, track those that still need to run in a separate map for further processing.
		for path, module := range modules {
			// Anything that is already applied is culled from the graph when running, so we ignore them here as well.
			switch {
			case module.Module.AssumeAlreadyApplied:
				removeDep = append(removeDep, path)
			case len(module.Dependencies) == 0:
				currentIterationDeploy = append(currentIterationDeploy, module.Module)
				removeDep = append(removeDep, path)
			default:
				next[path] = module
			}
		}

		// Go through the remaining module and remove the dependencies that were selected to run in this current
		// iteration.
		for _, module := range next {
			for _, path := range removeDep {
				_, hasDep := module.Dependencies[path]
				if hasDep {
					delete(module.Dependencies, path)
				}
			}
		}

		// Sort the group by path so that it is easier to read and test.
		sort.Slice(
			currentIterationDeploy,
			func(i, j int) bool {
				return currentIterationDeploy[i].Path < currentIterationDeploy[j].Path
			},
		)

		// Finally, update the trackers so that the next iteration runs.
		modules = next

		if len(currentIterationDeploy) > 0 {
			groups = append(groups, currentIterationDeploy)
		}
	}

	return groups
}

// Loop through the map of runningModules and for each module M:
//
//   - If dependencyOrder is NormalOrder, plug in all the modules M depends on into the Dependencies field and all the
//     modules that depend on M into the NotifyWhenDone field.
//   - If dependencyOrder is ReverseOrder, do the reverse.
//   - If dependencyOrder is IgnoreOrder, do nothing.
func (modules RunningModules) crossLinkDependencies(dependencyOrder DependencyOrder) (RunningModules, error) {
	for _, module := range modules {
		for _, dependency := range module.Module.Dependencies {
			runningDependency, hasDependency := modules[dependency.Path]
			if !hasDependency {
				return modules, errors.New(DependencyNotFoundWhileCrossLinkingError{module, dependency})
			}

			// TODO: Remove lint suppression
			switch dependencyOrder { //nolint:exhaustive
			case NormalOrder:
				module.Dependencies[runningDependency.Module.Path] = runningDependency
				runningDependency.NotifyWhenDone = append(runningDependency.NotifyWhenDone, module)
			case IgnoreOrder:
				// Nothing
			default:
				runningDependency.Dependencies[module.Module.Path] = module
				module.NotifyWhenDone = append(module.NotifyWhenDone, runningDependency)
			}
		}
	}

	return modules, nil
}

// RemoveFlagExcluded returns a cleaned-up map that only contains modules and
// dependencies that should not be excluded
func (modules RunningModules) RemoveFlagExcluded(r *report.Report, reportExperiment bool) (RunningModules, error) {
	var finalModules = make(map[string]*RunningModule)

	var errs []error

	for key, module := range modules {
		// Only add modules that should not be excluded
		if !module.FlagExcluded {
			finalModules[key] = &RunningModule{
				Module:         module.Module,
				Dependencies:   make(map[string]*RunningModule),
				DependencyDone: module.DependencyDone,
				Logger:         module.Logger,
				Err:            module.Err,
				NotifyWhenDone: module.NotifyWhenDone,
				Status:         module.Status,
			}

			// Only add dependencies that should not be excluded
			for path, dependency := range module.Dependencies {
				if !dependency.FlagExcluded {
					finalModules[key].Dependencies[path] = dependency
				}
			}
		} else if reportExperiment {
			run, err := r.EnsureRun(module.Module.Path)
			if err != nil {
				errs = append(errs, err)
				continue
			}

			if err := r.EndRun(
				run.Path,
				report.WithResult(report.ResultExcluded),
				report.WithReason(report.ReasonExcludeBlock),
			); err != nil {
				errs = append(errs, err)
			}
		}
	}

	if len(errs) > 0 {
		return finalModules, errors.Join(errs...)
	}

	return finalModules, nil
}

// Run the given map of module path to runningModule. To "run" a module, execute the RunTerragrunt command in its
// TerragruntOptions object. The modules will be executed in an order determined by their inter-dependencies, using
// as much concurrency as possible.
func (modules RunningModules) runModules(ctx context.Context, opts *options.TerragruntOptions, r *report.Report, parallelism int) error {
	var (
		waitGroup sync.WaitGroup
		semaphore = make(chan struct{}, parallelism) // Make a semaphore from a buffered channel
	)

	for _, module := range modules {
		waitGroup.Add(1)

		go func(module *RunningModule) {
			defer waitGroup.Done()

			module.runModuleWhenReady(ctx, opts, r, semaphore)
		}(module)
	}

	waitGroup.Wait()

	return modules.collectErrors()
}

// Collect the errors from the given modules and return a single error object to represent them, or nil if no errors
// occurred
func (modules RunningModules) collectErrors() error {
	var errs *errors.MultiError

	for _, module := range modules {
		if module.Err != nil {
			errs = errs.Append(module.Err)
		}
	}

	return errs.ErrorOrNil()
}
