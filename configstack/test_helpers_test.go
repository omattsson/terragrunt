package configstack_test

import (
	"context"
	"sort"
	"testing"

	"github.com/gruntwork-io/terragrunt/config"
	"github.com/gruntwork-io/terragrunt/configstack"
	"github.com/gruntwork-io/terragrunt/internal/errors"
	"github.com/gruntwork-io/terragrunt/internal/report"
	"github.com/gruntwork-io/terragrunt/options"
	"github.com/gruntwork-io/terragrunt/pkg/log"
	"github.com/gruntwork-io/terragrunt/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type TerraformModuleByPath configstack.TerraformModules

func (byPath TerraformModuleByPath) Len() int           { return len(byPath) }
func (byPath TerraformModuleByPath) Swap(i, j int)      { byPath[i], byPath[j] = byPath[j], byPath[i] }
func (byPath TerraformModuleByPath) Less(i, j int) bool { return byPath[i].Path < byPath[j].Path }

type RunningModuleByPath []*configstack.RunningModule

func (byPath RunningModuleByPath) Len() int      { return len(byPath) }
func (byPath RunningModuleByPath) Swap(i, j int) { byPath[i], byPath[j] = byPath[j], byPath[i] }
func (byPath RunningModuleByPath) Less(i, j int) bool {
	return byPath[i].Module.Path < byPath[j].Module.Path
}

// We can't use assert.Equals on TerraformModule or any data structure that contains it because it contains some
// fields (e.g. TerragruntOptions) that cannot be compared directly
func assertModuleListsEqual(t *testing.T, expectedModules configstack.TerraformModules, actualModules configstack.TerraformModules, messageAndArgs ...any) {
	t.Helper()

	if !assert.Len(t, actualModules, len(expectedModules), messageAndArgs...) {
		t.Logf("%s != %s", expectedModules, actualModules)
		return
	}

	sort.Sort(TerraformModuleByPath(expectedModules))
	sort.Sort(TerraformModuleByPath(actualModules))

	for i := range expectedModules {
		expected := expectedModules[i]
		actual := actualModules[i]
		assertModulesEqual(t, expected, actual, messageAndArgs...)
	}
}

// We can't use assert.Equals on TerraformModule because it contains some fields (e.g. TerragruntOptions) that cannot
// be compared directly
func assertModulesEqual(t *testing.T, expected *configstack.TerraformModule, actual *configstack.TerraformModule, messageAndArgs ...any) {
	t.Helper()

	if assert.NotNil(t, actual, messageAndArgs...) {
		// When comparing the TerragruntConfig objects, we need to normalize the dependency list to explicitly set the
		// expected to empty list when nil, as the parsing routine will set it to empty list instead of nil.
		if expected.Config.TerragruntDependencies == nil {
			expected.Config.TerragruntDependencies = config.Dependencies{}
		}
		if actual.Config.TerragruntDependencies == nil {
			actual.Config.TerragruntDependencies = config.Dependencies{}
		}
		assert.Equal(t, expected.Config, actual.Config, messageAndArgs...)

		assert.Equal(t, expected.Path, actual.Path, messageAndArgs...)
		assert.Equal(t, expected.AssumeAlreadyApplied, actual.AssumeAlreadyApplied, messageAndArgs...)
		assert.Equal(t, expected.FlagExcluded, actual.FlagExcluded, messageAndArgs...)

		assertOptionsEqual(t, *expected.TerragruntOptions, *actual.TerragruntOptions, messageAndArgs...)
		assertModuleListsEqual(t, expected.Dependencies, actual.Dependencies, messageAndArgs...)
	}
}

// We can't use assert.Equals on TerraformModule or any data structure that contains it (e.g. configstack.RunningModule) because it
// contains some fields (e.g. TerragruntOptions) that cannot be compared directly
func assertRunningModuleMapsEqual(t *testing.T, expectedModules map[string]*configstack.RunningModule, actualModules map[string]*configstack.RunningModule, doDeepCheck bool, messageAndArgs ...any) {
	t.Helper()

	if !assert.Len(t, actualModules, len(expectedModules), messageAndArgs...) {
		t.Logf("%v != %v", expectedModules, actualModules)
		return
	}

	for expectedPath, expectedModule := range expectedModules {
		actualModule, containsModule := actualModules[expectedPath]
		if assert.True(t, containsModule, messageAndArgs...) {
			assertRunningModulesEqual(t, expectedModule, actualModule, doDeepCheck, messageAndArgs...)
		}
	}
}

// We can't use assert.Equals on TerraformModule or any data structure that contains it (e.g. configstack.RunningModule) because it
// contains some fields (e.g. TerragruntOptions) that cannot be compared directly
func assertRunningModuleListsEqual(t *testing.T, expectedModules []*configstack.RunningModule, actualModules []*configstack.RunningModule, doDeepCheck bool, messageAndArgs ...any) {
	t.Helper()

	if !assert.Len(t, actualModules, len(expectedModules), messageAndArgs...) {
		t.Logf("%v != %v", expectedModules, actualModules)
		return
	}

	sort.Sort(RunningModuleByPath(expectedModules))
	sort.Sort(RunningModuleByPath(actualModules))

	for i := range expectedModules {
		expected := expectedModules[i]
		actual := actualModules[i]
		assertRunningModulesEqual(t, expected, actual, doDeepCheck, messageAndArgs...)
	}
}

// We can't use assert.Equals on TerraformModule or any data structure that contains it (e.g. configstack.RunningModule) because it
// contains some fields (e.g. TerragruntOptions) that cannot be compared directly
func assertRunningModulesEqual(t *testing.T, expected *configstack.RunningModule, actual *configstack.RunningModule, doDeepCheck bool, messageAndArgs ...any) {
	t.Helper()

	if assert.NotNil(t, actual, messageAndArgs...) {
		assert.Equal(t, expected.Status, actual.Status, messageAndArgs...)

		assertModulesEqual(t, expected.Module, actual.Module, messageAndArgs...)
		assertErrorsEqual(t, expected.Err, actual.Err, messageAndArgs...)

		// This ensures we don't end up in a circular loop, since there is a (intentional) circular dependency
		// between NotifyWhenDone and Dependencies
		if doDeepCheck {
			assertRunningModuleMapsEqual(t, expected.Dependencies, actual.Dependencies, false, messageAndArgs...)
			assertRunningModuleListsEqual(t, expected.NotifyWhenDone, actual.NotifyWhenDone, false, messageAndArgs...)
		}
	}
}

// We can't do a simple IsError comparison for configstack.UnrecognizedDependencyError because that error is a struct that
// contains an array, and in Go, trying to compare arrays gives a "comparing uncomparable type
// configstack.configstack.UnrecognizedDependencyError" panic. Therefore, we have to compare that error more manually.
func assertErrorsEqual(t *testing.T, expected error, actual error, messageAndArgs ...any) {
	t.Helper()

	actual = errors.Unwrap(actual)

	var unrecognizedDependencyError configstack.UnrecognizedDependencyError
	if ok := errors.As(expected, &unrecognizedDependencyError); ok {
		var actualUnrecognized configstack.UnrecognizedDependencyError
		ok = errors.As(actual, &actualUnrecognized)
		if assert.True(t, ok, messageAndArgs...) {
			assert.Equal(t, unrecognizedDependencyError, actualUnrecognized, messageAndArgs...)
		}
	} else {
		assert.True(t, errors.IsError(actual, expected), messageAndArgs...)
	}
}

// We can't do a direct comparison between TerragruntOptions objects because we can't compare Logger or RunTerragrunt
// instances. Therefore, we have to manually check everything else.
func assertOptionsEqual(t *testing.T, expected options.TerragruntOptions, actual options.TerragruntOptions, messageAndArgs ...any) {
	t.Helper()

	assert.Equal(t, expected.TerragruntConfigPath, actual.TerragruntConfigPath, messageAndArgs...)
	assert.Equal(t, expected.NonInteractive, actual.NonInteractive, messageAndArgs...)
	assert.Equal(t, expected.TerraformCliArgs, actual.TerraformCliArgs, messageAndArgs...)
	assert.Equal(t, expected.WorkingDir, actual.WorkingDir, messageAndArgs...)
}

// Return the absolute path for the given path
func canonical(t *testing.T, path string) string {
	t.Helper()

	out, err := util.CanonicalPath(path, ".")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func globCanonical(t *testing.T, path string) []string {
	t.Helper()

	out, err := util.GlobCanonicalPath(path, ".")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Create a mock TerragruntOptions object and configure its RunTerragrunt command to return the given error object. If
// the RunTerragrunt command is called, this method will also set the executed boolean to true.
func optionsWithMockTerragruntCommand(t *testing.T, terragruntConfigPath string, toReturnFromTerragruntCommand error, executed *bool) *options.TerragruntOptions {
	t.Helper()

	opts, err := options.NewTerragruntOptionsForTest(terragruntConfigPath)
	if err != nil {
		t.Fatalf("Error creating terragrunt options for test %v", err)
	}
	opts.RunTerragrunt = func(_ context.Context, _ log.Logger, _ *options.TerragruntOptions, _ *report.Report) error {
		*executed = true
		return toReturnFromTerragruntCommand
	}
	return opts
}

func assertMultiErrorContains(t *testing.T, actualError error, expectedErrors ...error) {
	t.Helper()

	multiError := new(errors.MultiError)
	errors.As(actualError, &multiError)
	require.NotNil(t, multiError, "Expected a MutliError, but got: %v", actualError)

	assert.Len(t, multiError.WrappedErrors(), len(expectedErrors))
	for _, expectedErr := range expectedErrors {
		found := false
		for _, actualErr := range multiError.WrappedErrors() {
			if errors.Is(expectedErr, actualErr) {
				found = true

				break
			}
		}
		assert.True(t, found, "Couldn't find expected error %v", expectedErr)
	}
}
