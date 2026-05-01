# PR 2: Add `azurehelper` package for Azure SDK interactions

## Context

Second of three incremental PRs to add Azure backend support to Terragrunt
([gruntwork-io/terragrunt#4307](https://github.com/gruntwork-io/terragrunt/issues/4307)).
Depends on PR 1 (experiment stubs) being merged first.

The maintainer's guidance: create a helper package that pattern-matches
[`awshelper`](https://github.com/gruntwork-io/terragrunt/tree/main/internal/awshelper) (4 files, 814 LOC) and
[`gcphelper`](https://github.com/gruntwork-io/terragrunt/tree/main/internal/gcphelper) (2 files, 441 LOC).
Tests must be gated behind a build tag, like
[`gcphelper/config_test.go`](https://github.com/gruntwork-io/terragrunt/blob/main/internal/gcphelper/config_test.go#L1)
uses `//go:build gcp`.

### Relationship to existing code

The `azurerm_storage` branch has a full implementation spread across 8 packages in
`internal/azure/` (14,269 LOC): `azureauth`, `azurehelper`, `azureutil`, `errorutil`,
`factory`, `implementations`, `interfaces`, `types`. This architecture uses factory/adapter/
interface patterns that don't match `awshelper`/`gcphelper` conventions.

**This PR does NOT refactor the existing code.** It is authored as a fresh branch off `main`,
using the existing branch as reference for Azure SDK calls, auth flows, and error handling —
but rewritten into a flat, builder-patterned package. The existing `internal/azure/` tree is
not included.

## Goal

All Azure SDK interaction lives in a single, flat `internal/azurehelper/` package. Builder
pattern for auth/client creation. Well-tested, build-tag gated. **No backend wiring yet** --
that's PR 3.

## Target structure

```
internal/azurehelper/
├── config.go               # AzureSessionConfig + AzureConfigBuilder (builder pattern)
├── storage_account.go      # Storage account CRUD operations
├── blob.go                 # Blob / container operations
├── resource_group.go       # Resource group ensure/check
├── rbac.go                 # Role assignment helpers (needed for use_azuread_auth bootstrap)
├── errors.go               # Error classification + wrapping
├── config_test.go          # //go:build azure
├── storage_account_test.go # //go:build azure
├── blob_test.go            # //go:build azure
├── rbac_test.go            # //go:build azure
└── resource_group_test.go  # //go:build azure
```

**Target: 8-10 files, ~2,000-3,000 LOC** (the original implementation was 14,269 LOC across 40 files in 8 packages).

## Detailed design

### config.go -- Builder pattern (mirrors awshelper/gcphelper)

The core pattern used by both `awshelper` and `gcphelper`:

```go
// awshelper pattern:
awshelper.NewAWSConfigBuilder().
    WithSessionConfig(cfg).
    WithEnv(env).
    WithIAMRoleOptions(opts).
    Build(ctx, logger)        // → aws.Config
    BuildS3Client(ctx, logger) // → *s3.Client

// gcphelper pattern:
gcphelper.NewGCPConfigBuilder().
    WithSessionConfig(cfg).
    WithEnv(env).
    Build(ctx)                  // → []option.ClientOption
    BuildGCSClient(ctx)         // → *storage.Client
```

The Azure equivalent:

```go
package azurehelper

type AzureSessionConfig struct {
    SubscriptionID     string
    TenantID           string
    ClientID           string
    ClientSecret       string
    StorageAccountName string
    ResourceGroupName  string
    ContainerName      string
    Location           string
    UseAzureADAuth     bool
    UseMSI             bool
    SasToken           string
    AccessKey          string
    CloudEnvironment   string // "public", "government", "china"
}

type AzureConfigBuilder struct {
    sessionConfig  AzureSessionConfig
    env            map[string]string
}

func NewAzureConfigBuilder() *AzureConfigBuilder

func (b *AzureConfigBuilder) WithSessionConfig(cfg AzureSessionConfig) *AzureConfigBuilder
func (b *AzureConfigBuilder) WithEnv(env map[string]string) *AzureConfigBuilder

// Build resolves credentials and returns a config object.
// Auth resolution order:
//   1. SAS token (data-plane only, no credential needed)
//   2. Access key
//   3. Service principal (client_id + client_secret + tenant_id)
//   4. MSI (managed identity)
//   5. Azure AD / CLI (az login)
//   6. Azure SDK default credential chain
func (b *AzureConfigBuilder) Build(ctx context.Context, logger log.Logger) (*AzureConfig, error)

// BuildBlobClient creates an Azure Blob Storage client.
func (b *AzureConfigBuilder) BuildBlobClient(ctx context.Context, logger log.Logger) (*BlobClient, error)

// BuildStorageAccountClient creates an ARM storage account management client.
func (b *AzureConfigBuilder) BuildStorageAccountClient(ctx context.Context, logger log.Logger) (*StorageAccountClient, error)

// AzureConfig holds the resolved credential and session metadata.
// Analogous to aws.Config (returned by awshelper.Build) or []option.ClientOption
// (returned by gcphelper.Build). The BuildXxxClient methods use this internally;
// callers typically use BuildBlobClient/BuildStorageAccountClient directly.
type AzureConfig struct {
    Credential     azcore.TokenCredential // nil for SAS token / access key auth
    SasToken       string                 // non-empty only for SAS auth
    AccessKey      string                 // non-empty only for access key auth
    SubscriptionID string
    TenantID       string
    AccountName    string
    ResourceGroup  string
    CloudConfig    cloud.Configuration    // public, government, or china
}
```

**Auth resolution** happens inside `Build()`, not in a separate package. Compare how
`awshelper/config.go` handles credential chains entirely within the builder (env provider →
static file → STS assume role → Web identity token). No separate `awsauth` package exists.

Environment variable fallbacks to support (matching Terraform's azurerm backend):

```
ARM_SUBSCRIPTION_ID, AZURE_SUBSCRIPTION_ID
ARM_TENANT_ID, AZURE_TENANT_ID
ARM_CLIENT_ID, AZURE_CLIENT_ID
ARM_CLIENT_SECRET, AZURE_CLIENT_SECRET
ARM_SAS_TOKEN, AZURE_STORAGE_SAS_TOKEN
ARM_ACCESS_KEY, AZURE_STORAGE_KEY
ARM_USE_MSI, ARM_USE_OIDC
```

### storage_account.go -- Storage account operations

```go
type StorageAccountClient struct {
    credential     azcore.TokenCredential
    subscriptionID string
    resourceGroup  string
    accountName    string
}

func (c *StorageAccountClient) Exists(ctx context.Context) (bool, error)
func (c *StorageAccountClient) Create(ctx context.Context, logger log.Logger, cfg StorageAccountConfig) error
func (c *StorageAccountClient) Delete(ctx context.Context, logger log.Logger) error
func (c *StorageAccountClient) EnableVersioning(ctx context.Context, logger log.Logger) error
func (c *StorageAccountClient) IsVersioningEnabled(ctx context.Context) (bool, error)
func (c *StorageAccountClient) EnableSoftDelete(ctx context.Context, logger log.Logger, retentionDays int) error
func (c *StorageAccountClient) GetKeys(ctx context.Context) ([]string, error)

type StorageAccountConfig struct {
    Name              string
    ResourceGroupName string
    Location          string
    AccountKind       string // "StorageV2", "BlobStorage", etc.
    AccountTier       string // "Standard", "Premium"
    ReplicationType   string // "LRS", "GRS", "ZRS", etc.
    AccessTier        string // "Hot", "Cool"
    EnableVersioning  bool
    Tags              map[string]string
}
```

No interfaces, no factory, no adapters. One concrete struct with direct methods -- exactly
like the S3 client in `internal/remotestate/backend/s3/client.go` which embeds its config and
holds `*s3.Client` + `*dynamodb.Client` directly.

### blob.go -- Blob and container operations

```go
type BlobClient struct {
    client        *azblob.Client // or *container.Client
    containerName string
    accountName   string
}

func (c *BlobClient) GetObject(ctx context.Context, key string) (io.ReadCloser, error)
func (c *BlobClient) ContainerExists(ctx context.Context) (bool, error)
func (c *BlobClient) CreateContainer(ctx context.Context, logger log.Logger) error
func (c *BlobClient) DeleteBlob(ctx context.Context, key string) error
func (c *BlobClient) CopyBlob(ctx context.Context, srcKey, dstContainer, dstKey string) error
func (c *BlobClient) ListBlobs(ctx context.Context, prefix string) ([]string, error)
```

The `GetObject` signature is what `pkg/config/dependency.go` calls for
`--dependency-fetch-output-from-state`. Keep it simple -- return `io.ReadCloser`, let the
caller parse JSON.

> **Note:** The existing branch uses `GetObject(ctx, *GetObjectInput) (*GetObjectOutput, error)`
> with wrapper structs mirroring the S3 SDK pattern. This is a deliberate simplification --
> Azure blob downloads only need a key string (the container is on the client). The simpler
> signature reduces API surface. PR 3's `dependency.go` will call `blobClient.GetObject(ctx, key)`
> directly.

### resource_group.go -- Resource group management

```go
func (c *StorageAccountClient) EnsureResourceGroup(ctx context.Context, logger log.Logger, location string) error
```

This can be a method on `StorageAccountClient` since it shares the same credential and
subscription scope. Or a standalone function -- keep it flat.

### rbac.go -- Role assignment helpers

Needed during bootstrap when `use_azuread_auth = true` to assign
"Storage Blob Data Contributor" / "Storage Blob Data Owner" roles.

Functions take `*AzureConfig` (from the builder) rather than raw `azcore.TokenCredential`,
keeping credentials encapsulated within the builder pattern:

```go
func AssignRoleIfMissing(ctx context.Context, logger log.Logger, cfg *AzureConfig,
    scope, principalID, roleName string) error

func HasRoleAssignment(ctx context.Context, cfg *AzureConfig,
    scope, principalID, roleDefinitionID string) (bool, error)

func RemoveRole(ctx context.Context, logger log.Logger, cfg *AzureConfig,
    scope, principalID, roleName string) error
```

### errors.go -- Error classification

Flatten `errorutil/`, `azureutil/errorhandling.go`, `azureutil/error_types.go` into simple functions:

```go
func ClassifyError(err error) string  // "authentication", "permissions", "transient", etc.
func IsRetryable(err error) bool
func IsNotFound(err error) bool
func WrapError(err error, op string) error
```

No `ErrorMetrics`, no `WithErrorHandling` wrapper, no telemetry package. S3 and GCS don't
have this level of error infrastructure.

**Retry strategy:** `IsRetryable()` is consumed by PR 3's backend layer, which wraps
operations in retry loops (similar to `s3/client.go`'s retry logic). This package provides
the classification; the backend provides the retry loop.

## What gets consolidated

| Original package (8 packages, 14,269 LOC) | Fate |
|-------------------------------------------|------|
| `azure/interfaces/` (6 files, 1,021 LOC) | **Deleted** -- no interfaces, concrete types only |
| `azure/factory/` (3 files, 1,448 LOC) | **Deleted** -- builder on `AzureConfigBuilder` replaces it |
| `azure/implementations/` (5 files, 1,604 LOC) | **Deleted** -- methods live directly on client structs |
| `azure/types/` (6 files, 732 LOC) | **Inlined** -- simple config structs in the files that use them |
| `azure/errorutil/` (1 file, 446 LOC) | **Merged** into `errors.go` |
| `azure/azureutil/` (4 files, 1,374 LOC) | **Cut** -- telemetry/error-handling wrappers are overhead |
| `azure/azureauth/` (3 files, 948 LOC) | **Merged** into `config.go` builder |
| `azure/azurehelper/` (14 files, 6,696 LOC) | **Slimmed** -- becomes the core of the new package |

**Key architectural differences from the original:**

1. **No interfaces package** -- neither `awshelper` nor `gcphelper` define interfaces.
   The backend package uses the concrete client types directly.
2. **No factory/adapter pattern** -- the builder creates clients directly.
   `awshelper.NewAWSConfigBuilder().BuildS3Client()` is the pattern.
3. **No telemetry/metrics infrastructure** -- neither AWS nor GCS backends have this.
   Add it later if the maintainers want it.
4. **No separate auth package** -- auth is internal to the builder, same as AWS/GCP.
5. **Flat package** -- all files at one level, no subdirectories.

## Test strategy

All test files use `//go:build azure` on line 1 (matching `//go:build gcp` in gcphelper):

```go
//go:build azure

package azurehelper_test

import (
    "testing"
    // ...
)
```

- **Unit tests**: Mock at the Azure SDK HTTP transport level using `policy.ClientOptions` with
  a test `Transporter`. Test auth resolution, config parsing, error classification.
- **Integration tests**: Hit real Azure (gated by build tag, not run in CI initially).
  Test storage account CRUD, blob operations, RBAC assignment.
- The maintainer said: "You don't need to worry about hooking up tests to CI. We can take care of that."

## go.mod dependencies

This PR will add Azure SDK dependencies:

```
github.com/Azure/azure-sdk-for-go/sdk/azcore
github.com/Azure/azure-sdk-for-go/sdk/azidentity
github.com/Azure/azure-sdk-for-go/sdk/storage/azblob
github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/storage/armstorage
github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources
github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/authorization/armauthorization/v2
```

## What NOT to include

- No changes to `internal/remotestate/backend/azurerm/` (that's PR 3)
- No changes to `pkg/config/dependency.go` (that's PR 3)
- No changes to documentation beyond what's in PR 1
- No `internal/azure/` directory tree -- everything is in `internal/azurehelper/`

## Acceptance criteria

- `go build ./internal/azurehelper/...` succeeds
- `go vet ./internal/azurehelper/...` passes
- `go test -tags azure ./internal/azurehelper/...` passes (with Azure credentials available)
- `go test ./...` passes without the azure tag (tests are skipped, no compile errors)
- Package follows the flat structure of `awshelper`/`gcphelper`
- No interfaces package, no factory pattern, no adapter layer
- Builder pattern for client creation matches `awshelper`/`gcphelper`

## References

- Maintainer feedback: https://github.com/gruntwork-io/terragrunt/issues/4307#issuecomment-4354333422
- `awshelper` pattern: [`internal/awshelper/`](internal/awshelper/) (4 files, 814 LOC, builder pattern)
- `gcphelper` pattern: [`internal/gcphelper/`](internal/gcphelper/) (2 files, 441 LOC, builder pattern)
- GCP build tag example: [`internal/gcphelper/config_test.go#L1`](internal/gcphelper/config_test.go) (`//go:build gcp`)
- S3 client pattern: [`internal/remotestate/backend/s3/client.go`](internal/remotestate/backend/s3/client.go)
