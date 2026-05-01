# PR 3: Wire azurehelper into the backend system + dependency fetching

## Context

Third and final incremental PR to add Azure backend support to Terragrunt
([gruntwork-io/terragrunt#4307](https://github.com/gruntwork-io/terragrunt/issues/4307)).
Depends on PR 1 (experiment stubs) and PR 2 (azurehelper package) being merged first.

### Relationship to existing code

The `azurerm_storage` branch has a full backend implementation in
`internal/remotestate/backend/azurerm/` (8,962 LOC, 12 files) with factory/interfaces/
telemetry/error-handler infrastructure, plus 2,218 LOC of integration tests and test fixtures.

**This PR replaces the existing backend with a thin S3/GCS-parallel design.** It is authored
as a fresh branch off `main` (with PR 1 + PR 2 merged), using the existing branch as reference
for Azure API calls, error handling, and test scenarios — but rewritten to match the S3/GCS
backend structure. The `internal/azure/` tree (factory, interfaces, implementations, types,
errorutil, azureutil) is not included; all Azure SDK interaction goes through `azurehelper`
from PR 2.

Key architectural changes from the existing branch:
- `NewBackend()` takes zero args (matching S3/GCS), not `NewBackend(cfg *BackendConfig)`
- No `interfaces.AzureServiceContainer` or `interfaces.ServiceFactory` — client struct holds
  `azurehelper.BlobClient` and `azurehelper.StorageAccountClient` directly
- No `AzureTelemetryCollector` or `azureutil.OperationHandler` — errors handled inline
- Config uses `ExtendedRemoteStateConfigAzurerm` with `mapstructure` tags (matching S3/GCS),
  not the current 588-LOC config with `StorageAccountBootstrapConfig` substruct

## Goal

Wire the `azurehelper` package into Terragrunt's backend system so that
`--experiment azure-backend` enables full bootstrap, delete, migrate, and dependency output
fetching for `azurerm` backends. Also add complete documentation.

## Target structure

```
internal/remotestate/backend/azurerm/
├── backend.go              # Backend struct implementing backend.Backend
├── client.go               # Client struct (wraps azurehelper clients + config)
├── config.go               # ExtendedRemoteStateConfigAzurerm, parsing, GetAzureSessionConfig()
├── remote_state_config.go  # Config key constants, FilterOutTerragruntKeys
├── errors.go               # Backend-specific error types
├── config_test.go          # //go:build azure
└── backend_test.go         # //go:build azure
```

**Target: 6-8 files in the backend package, plus integration tests (~4,000-5,000 LOC total)**

## Detailed design

### backend.go -- Mirrors S3/GCS pattern exactly

The S3 backend pattern at `internal/remotestate/backend/s3/backend.go`:

```go
// S3 pattern - each method does:
// 1. Parse config → ExtendedRemoteStateConfigS3
// 2. Create client → NewClient(ctx, logger, extCfg, opts)
// 3. Call client method
func (b *Backend) Bootstrap(ctx context.Context, logger log.Logger, backendConfig backend.Config, opts *backend.Options) error {
    extS3Cfg, err := Config(backendConfig).ExtendedS3Config(logger)
    client, err := NewClient(ctx, logger, extS3Cfg, opts)
    // ... client operations
}
```

The Azure backend follows the same structure:

```go
package azurerm

type Backend struct {
    *backend.CommonBackend
}

func NewBackend() *Backend {
    return &Backend{CommonBackend: backend.NewCommonBackend("azurerm")}
}

func (b *Backend) Name() string { return "azurerm" }

func (b *Backend) NeedsBootstrap(ctx context.Context, logger log.Logger,
    backendConfig backend.Config, opts *backend.Options) (bool, error) {
    // Gate behind experiment
    if !opts.Experiments.Evaluate(experiment.AzureBackend) {
        return false, nil
    }

    extCfg, err := Config(backendConfig).ParseExtendedAzureConfig(logger)
    if err != nil { return false, err }

    client, err := NewClient(ctx, logger, extCfg, opts)
    if err != nil { return false, err }

    // Check if storage account + container exist
    return client.NeedsBootstrap(ctx, logger)
}

func (b *Backend) Bootstrap(ctx context.Context, logger log.Logger,
    backendConfig backend.Config, opts *backend.Options) error {
    if !opts.Experiments.Evaluate(experiment.AzureBackend) {
        return nil
    }

    extCfg, err := Config(backendConfig).ParseExtendedAzureConfig(logger)
    if err != nil { return err }

    client, err := NewClient(ctx, logger, extCfg, opts)
    if err != nil { return err }

    // Use bucket-level mutex to prevent parallel bootstrap (same pattern as S3)
    mutex := backend.GetBucketMutex(extCfg.StorageAccountName)
    mutex.Lock()
    defer mutex.Unlock()

    if backend.IsConfigInited(backendConfig) {
        return nil
    }

    if err := client.CreateStorageAccountIfNecessary(ctx, logger); err != nil {
        return err
    }
    if err := client.CreateContainerIfNecessary(ctx, logger); err != nil {
        return err
    }
    if err := client.EnableVersioningIfNecessary(ctx, logger); err != nil {
        return err
    }
    if extCfg.UseAzureADAuth {
        if err := client.AssignRBACRolesIfNecessary(ctx, logger); err != nil {
            return err
        }
    }

    backend.SetConfigInited(backendConfig)
    return nil
}

func (b *Backend) IsVersionControlEnabled(ctx context.Context, logger log.Logger,
    backendConfig backend.Config, opts *backend.Options) (bool, error) { ... }

func (b *Backend) Migrate(ctx context.Context, logger log.Logger,
    srcConfig, dstConfig backend.Config, opts *backend.Options) error { ... }

func (b *Backend) Delete(ctx context.Context, logger log.Logger,
    backendConfig backend.Config, opts *backend.Options) error { ... }

func (b *Backend) DeleteBucket(ctx context.Context, logger log.Logger,
    backendConfig backend.Config, opts *backend.Options) error { ... }

func (b *Backend) DeleteStorageAccount(ctx context.Context, logger log.Logger,
    backendConfig backend.Config, opts *backend.Options) error { ... }

func (b *Backend) GetTFInitArgs(backendConfig backend.Config) map[string]any {
    return Config(backendConfig).GetTFInitArgs()
}
```

### client.go -- Wraps azurehelper clients

Pattern from S3 (`internal/remotestate/backend/s3/client.go`):

```go
// S3 client pattern:
type Client struct {
    *ExtendedRemoteStateConfigS3
    s3Client     *s3.Client
    dynamoClient *dynamodb.Client
    awsConfig    aws.Config
}

func NewClient(ctx, logger, config, opts) (*Client, error) {
    awsConfig := awshelper.NewAWSConfigBuilder().
        WithSessionConfig(config.GetAwsSessionConfig()).
        WithEnv(opts.Env).
        WithIAMRoleOptions(opts.IAMRoleOptions).
        Build(ctx, logger)
    // ...
}
```

The Azure equivalent:

```go
type Client struct {
    *ExtendedRemoteStateConfigAzurerm
    blobClient           *azurehelper.BlobClient
    storageAccountClient *azurehelper.StorageAccountClient
}

func NewClient(ctx context.Context, logger log.Logger,
    config *ExtendedRemoteStateConfigAzurerm, opts *backend.Options) (*Client, error) {

    builder := azurehelper.NewAzureConfigBuilder().
        WithSessionConfig(config.GetAzureSessionConfig()).
        WithEnv(opts.Env)

    blobClient, err := builder.BuildBlobClient(ctx, logger)
    if err != nil { return nil, err }

    storageAccountClient, err := builder.BuildStorageAccountClient(ctx, logger)
    if err != nil { return nil, err }

    return &Client{
        ExtendedRemoteStateConfigAzurerm: config,
        blobClient:                       blobClient,
        storageAccountClient:             storageAccountClient,
    }, nil
}

// High-level operations used by backend.go
func (c *Client) NeedsBootstrap(ctx context.Context, logger log.Logger) (bool, error) {
    acctExists, err := c.storageAccountClient.Exists(ctx)
    if err != nil { return false, err }
    if !acctExists { return true, nil }

    containerExists, err := c.blobClient.ContainerExists(ctx)
    if err != nil { return false, err }
    return !containerExists, nil
}

func (c *Client) CreateStorageAccountIfNecessary(ctx context.Context, logger log.Logger) error
func (c *Client) CreateContainerIfNecessary(ctx context.Context, logger log.Logger) error
func (c *Client) EnableVersioningIfNecessary(ctx context.Context, logger log.Logger) error
func (c *Client) AssignRBACRolesIfNecessary(ctx context.Context, logger log.Logger) error
func (c *Client) DeleteBlob(ctx context.Context, key string) error
func (c *Client) MigrateBlob(ctx context.Context, logger log.Logger, dstClient *Client) error
```

### config.go -- Extended config with session config getter

Pattern from S3 (`internal/remotestate/backend/s3/config.go`):

```go
// S3 pattern:
type ExtendedRemoteStateConfigS3 struct {
    RemoteStateConfigS3
    // S3-specific fields...
}
func (c *ExtendedRemoteStateConfigS3) GetAwsSessionConfig() awshelper.AwsSessionConfig
```

The Azure equivalent:

```go
type RemoteStateConfigAzurerm struct {
    StorageAccountName string `mapstructure:"storage_account_name"`
    ContainerName      string `mapstructure:"container_name"`
    Key                string `mapstructure:"key"`
    SubscriptionID     string `mapstructure:"subscription_id"`
    ResourceGroupName  string `mapstructure:"resource_group_name"`
    TenantID           string `mapstructure:"tenant_id"`
    ClientID           string `mapstructure:"client_id"`
    ClientSecret       string `mapstructure:"client_secret"`
    UseAzureADAuth     bool   `mapstructure:"use_azuread_auth"`
    UseMSI             bool   `mapstructure:"use_msi"`
    SasToken           string `mapstructure:"sas_token"`
    AccessKey          string `mapstructure:"access_key"`
    CloudEnvironment   string `mapstructure:"environment"`
}

type ExtendedRemoteStateConfigAzurerm struct {
    RemoteStateConfigAzurerm

    // Terragrunt-only fields (not passed to terraform init)
    CreateStorageAccountIfNotExists bool   `mapstructure:"create_storage_account_if_not_exists"`
    Location                       string `mapstructure:"location"`
    AccountKind                    string `mapstructure:"account_kind"`
    AccountTier                    string `mapstructure:"account_tier"`
    ReplicationType                string `mapstructure:"replication_type"`
    EnableVersioning               bool   `mapstructure:"enable_versioning"`
}

func (c *ExtendedRemoteStateConfigAzurerm) GetAzureSessionConfig() azurehelper.AzureSessionConfig {
    return azurehelper.AzureSessionConfig{
        SubscriptionID:     c.SubscriptionID,
        TenantID:           c.TenantID,
        ClientID:           c.ClientID,
        ClientSecret:       c.ClientSecret,
        StorageAccountName: c.StorageAccountName,
        ResourceGroupName:  c.ResourceGroupName,
        ContainerName:      c.ContainerName,
        Location:           c.Location,
        UseAzureADAuth:     c.UseAzureADAuth,
        UseMSI:             c.UseMSI,
        SasToken:           c.SasToken,
        AccessKey:          c.AccessKey,
        CloudEnvironment:   c.CloudEnvironment,
    }
}
```

### Dependency fetching -- `pkg/config/dependency.go`

Update `getTerragruntOutputJSONFromRemoteStateAzurerm()` to use the builder pattern,
matching the S3 pattern:

```go
// S3 pattern (lines 1295-1348 in dependency.go):
func getTerragruntOutputJSONFromRemoteStateS3(...) {
    s3ConfigExtended := s3backend.Config(remoteState.BackendConfig).ParseExtendedS3Config()
    sessionConfig := s3ConfigExtended.GetAwsSessionConfig()
    s3Client := awshelper.NewAWSConfigBuilder().
        WithSessionConfig(sessionConfig).
        WithEnv(pctx.Env).
        WithIAMRoleOptions(pctx.IAMRoleOptions).
        BuildS3Client(ctx, logger)
    result := s3Client.GetObject(ctx, &s3.GetObjectInput{
        Bucket: &s3ConfigExtended.RemoteStateConfigS3.Bucket,
        Key:    &s3ConfigExtended.RemoteStateConfigS3.Key,
    })
    // ... parse JSON
}
```

The Azure equivalent:

```go
func getTerragruntOutputJSONFromRemoteStateAzurerm(ctx context.Context, logger log.Logger,
    remoteState *remotestate.RemoteState, env map[string]string) ([]byte, error) {

    extCfg, err := azurerm.Config(remoteState.BackendConfig).ParseExtendedAzureConfig(logger)
    if err != nil { return nil, err }

    blobClient, err := azurehelper.NewAzureConfigBuilder().
        WithSessionConfig(extCfg.GetAzureSessionConfig()).
        WithEnv(env).
        BuildBlobClient(ctx, logger)
    if err != nil { return nil, err }

    body, err := blobClient.GetObject(ctx, extCfg.Key)
    if err != nil { return nil, err }
    defer body.Close()

    data, err := io.ReadAll(body)
    if err != nil { return nil, err }

    var state struct {
        Outputs map[string]interface{} `json:"outputs"`
    }
    if err := json.Unmarshal(data, &state); err != nil {
        return nil, err
    }

    return json.Marshal(state.Outputs)
}
```

### Documentation updates

Complete the documentation started in PR 1:

1. **`docs/src/content/docs/03-features/01-units/03-state-backend.mdx`**
   - Full Azure section: quickstart, required config table, authentication methods,
     state locking (blob leases are automatic), bootstrapping details
   - Note that state locking uses Azure blob leases automatically (no DynamoDB equivalent needed)

2. **`docs/src/data/commands/backend/bootstrap.mdx`**
   - Azure example in frontmatter
   - Azure Backend section: what gets created, RBAC requirements

3. **`docs/src/data/commands/backend/delete.mdx`**
   - Azure example in frontmatter
   - Azure Backend section: blob deletion with confirmation

4. **`docs/src/data/commands/backend/migrate.mdx`**
   - Azure example in frontmatter
   - Azure Backend section: SDK-direct blob copy with verification

5. **`docs/src/content/docs/06-troubleshooting/04-azure-backend.mdx`**
   - Troubleshooting page covering: auth errors, missing config, storage account errors,
     permission errors, transient errors, debugging tips

6. **`docs/src/data/flags/dependency-fetch-output-from-state.mdx`**
   - Update to mention Azure Storage alongside S3

## State locking

Azure uses blob leases for state locking. This is automatic via Terraform/OpenTofu's native
`azurerm` backend -- no Terragrunt-side lock infrastructure is needed (unlike S3 which requires
DynamoDB). The backend does not need to create or manage lock resources.

## Experiment gate

All backend methods (`Bootstrap`, `NeedsBootstrap`, `Delete`, `Migrate`, etc.) must check
the experiment gate. The behavior differs by method:

- **`NeedsBootstrap`** — returns `false, nil` (silent skip). This allows Terraform/OpenTofu's
  native azurerm backend to handle `init` without Terragrunt's bootstrap layer.
- **`Bootstrap`** — returns an explicit error: `"the azurerm backend requires the 'azure-backend'
  experiment to be enabled"`. This is safer than a silent no-op because Bootstrap is only called
  when `NeedsBootstrap` returned true — if someone reaches Bootstrap without the experiment, it's
  a bug worth surfacing.
- **`Delete`, `Migrate`, `DeleteBucket`, `DeleteStorageAccount`** — return an explicit error.
  These are destructive operations the user explicitly invoked; silent no-ops would be confusing.
- **`GetTFInitArgs`** — no gate. This runs unconditionally during `terraform init` regardless of
  experiment state. It must filter Terragrunt-only keys so Terraform doesn't reject them.
- **`IsVersionControlEnabled`** — returns `false, nil` (silent skip).

```go
// Example for Bootstrap:
if !opts.Experiments.Evaluate(experiment.AzureBackend) {
    return errors.Errorf("the azurerm backend requires the 'azure-backend' experiment to be enabled")
}
```

## What NOT to include

- No changes to `internal/azurehelper/` (that was PR 2)
- No `internal/azure/` directory tree (deleted / not created)
- No factory, adapter, or interfaces packages

## Integration tests

### Test file: `test/integration_azure_test.go`

Gated with `//go:build azure` on line 1. Tests require Azure credentials and the following
environment variables:

```bash
TERRAGRUNT_AZURE_TEST_STORAGE_ACCOUNT  # pre-existing storage account for tests
TERRAGRUNT_AZURE_TEST_LOCATION         # Azure region (defaults to "westeurope")
```

Each test generates a unique container name, runs operations, and cleans up via `t.Cleanup()`.
All Terragrunt commands must include `--experiment azure-backend`.

### Required test cases

These cover the full backend lifecycle and match the existing S3/GCS integration test patterns
(see `test/integration_aws_test.go` for reference):

| Test | What it verifies |
|------|-----------------|
| `TestAzureBackendBootstrap` | `terragrunt run -- init` creates container when `create_storage_account_if_not_exists = true`, skips on second run |
| `TestAzureBackendBootstrapExistingAccount` | Bootstrap against pre-existing storage account — only creates the container |
| `TestAzureBackendBootstrapIdempotent` | Running bootstrap twice is a no-op the second time |
| `TestAzureBackendDelete` | `terragrunt backend delete` removes the state blob, container still exists |
| `TestAzureBackendMigration` | `terragrunt backend migrate` copies blob to destination container, verifies content, removes source |
| `TestAzureBackendMigrationWithUnits` | Migration across multiple units in a stack |
| `TestAzureOutputFromRemoteState` | `dependency` block reads outputs from Azure remote state via `terraform output` |
| `TestAzureOutputFromDependency` | `dependency` block with `--dependency-fetch-output-from-state` reads blob directly |
| `TestAzureParallelStateInit` | Multiple units with the same backend bootstrap in parallel without races |
| `TestAzureBackendVersioning` | `IsVersionControlEnabled` returns correct result based on blob versioning status |
| `TestAzureRBACRoleAssignment` | Bootstrap with `use_azuread_auth = true` assigns Storage Blob Data roles |
| `TestAzureBlobOperations` | Unit-level blob CRUD: upload, download, list, delete |
| `TestAzureStorageContainerCreation` | Container creation with valid/invalid names |
| `TestAzureBackendExperimentGate` | Without `--experiment azure-backend`, backend is a no-op (falls through to native) |

### Test fixtures

Each test scenario needs a fixture directory under `test/fixtures/`:

```
test/fixtures/
├── azure-backend/              # Basic bootstrap test
│   ├── terragrunt.hcl          # remote_state with azurerm backend
│   ├── common.hcl              # shared config (account name, location from env)
│   ├── unit1/terragrunt.hcl
│   └── unit2/terragrunt.hcl
├── azure-backend-migrate/      # Migration test (source + destination configs)
│   ├── source/terragrunt.hcl
│   └── destination/terragrunt.hcl
├── azure-output-from-remote-state/  # dependency output via terraform output
│   ├── dependency/terragrunt.hcl    # produces outputs
│   └── consumer/terragrunt.hcl      # reads outputs via dependency block
├── azure-output-from-dependency/    # dependency output via direct state read
│   ├── dependency/terragrunt.hcl
│   └── consumer/terragrunt.hcl
└── azure-parallel-state-init/       # parallel bootstrap race condition test
    ├── unit1/terragrunt.hcl
    ├── unit2/terragrunt.hcl
    └── unit3/terragrunt.hcl
```

### Test helpers: `test/helpers/azuretest/`

Shared helper functions for Azure integration tests:

```go
package azuretest

// CreateTestStorageAccount creates a temporary storage account for testing.
// Returns the account name and a cleanup function.
func CreateTestStorageAccount(t *testing.T, location string) (string, func())

// CleanupContainer deletes a blob container if it exists.
func CleanupContainer(t *testing.T, accountName, containerName string)

// UploadTestBlob uploads a test blob to verify read operations.
func UploadTestBlob(t *testing.T, accountName, containerName, key string, data []byte)

// BlobExists checks whether a blob exists in a container.
func BlobExists(t *testing.T, accountName, containerName, key string) bool
```

### Running tests locally

```bash
# Login to Azure
az login

# Set required env vars
export TERRAGRUNT_AZURE_TEST_STORAGE_ACCOUNT="mytestaccount"
export TERRAGRUNT_AZURE_TEST_LOCATION="westeurope"

# Run all Azure integration tests
go test -tags azure -v -timeout 30m ./test/... -run TestAzure

# Run a specific test
go test -tags azure -v -timeout 10m ./test/... -run TestAzureBackendBootstrap
```

The maintainer said: "You don't need to worry about hooking up tests to CI. We can take care of that."

## Test strategy (unit tests)

In addition to integration tests, the backend package includes unit tests:

- **`config_test.go`**: Verify mapstructure tags, `GetTFInitArgs` key filtering,
  `GetAzureSessionConfig()` field mapping, config validation
- **`backend_test.go`**: Test experiment gate behavior, config parsing edge cases

These do NOT require Azure credentials and should NOT use the `//go:build azure` tag.

## Acceptance criteria

- `terragrunt --experiment azure-backend run -- plan` with an `azurerm` remote_state block:
  - Creates storage account + container if `create_storage_account_if_not_exists = true`
  - Skips bootstrap if they already exist
  - Passes correct config to `terraform init`
- `terragrunt --experiment azure-backend backend delete` removes the state blob
- `terragrunt --experiment azure-backend backend migrate` copies blobs between containers
- `--dependency-fetch-output-from-state` reads Azure blob state directly
- Without `--experiment azure-backend`, all operations fall through to native backend
- `go build ./...` passes
- `go vet ./...` passes
- `go test ./internal/remotestate/backend/azurerm/...` passes (unit tests, no tag needed)
- `go test -tags azure ./test/... -run TestAzure` passes (integration tests, credentials needed)
- Documentation renders correctly

## References

- Maintainer feedback: https://github.com/gruntwork-io/terragrunt/issues/4307#issuecomment-4354333422
- S3 backend (pattern to follow): [`internal/remotestate/backend/s3/`](internal/remotestate/backend/s3/)
- GCS backend (pattern to follow): [`internal/remotestate/backend/gcs/`](internal/remotestate/backend/gcs/)
- S3 dependency fetching: `pkg/config/dependency.go:1295-1348`
- Backend registration: [`internal/remotestate/remote_state.go`](internal/remotestate/remote_state.go)
- Experiment evaluation: `opts.Experiments.Evaluate(experiment.AzureBackend)`
