# Supervisor network migration container

The Supervisor migration framework runs as a separate process/container in the
NCP Pod. It owns the cluster-scoped `migration.network.vmware.com/v1alpha1` resources
`ControlPlaneNetworkTransition` and `NamespaceNetworkTransition`.

## Branch scope

This branch, `topic/xl018891/migration-fabric-preparation-wbs-1.4.3`, builds on
`topic/xl018891/migration-infrastructure-start-wbs-1.2.1` at `eb931da9`.
It adds the WBS 1.4.3 fabric service, typed NSX resource adapter, typed Net Operator
source allocation service and SNAT bootstrap coordination. It retains the parent's
infrastructure readiness/project checks and durable action/recovery runtime.

This is service implementation, not end-to-end WBS 1.4.3 completion. Complete
Evaluate/Derive handlers, inspected topology/approved-plan synthesis, and verified
source-IPAM/target-LB integration must supply the contracts described below before
production registration. No new action handler is registered by this branch.

## Framework and services

- `cmd_migration`: independent controller manager; no normal NSX controllers,
  webhook servers, `electMaster` calls, or `nsx-operator-role` label changes.
- `pkg/apis/migration/v1alpha1`: Shared orchestration APIs in migration.network.vmware.com/v1alpha1.
- `pkg/migration/controllers` and `pkg/migration/transition`: live dependency gates,
  action checkpoints, namespace rollback, infrastructure forward retry, and
  irreversible commit/finalize recovery.
- `pkg/migration.SetupWithManager`: registers both stages with a shared dispatcher
  when enabled; no intermediate Operator or unused NSX service is constructed.
- `pkg/nsx/services/migration.Service`: lazy NSX connectivity available for future
  handlers. Connections are process-local, not shared between containers.

Migration dispatch defaults to disabled: no transition controllers, watches, or
leader election are started. Disabled startup loads local Kubernetes REST
configuration without probing API-server health. Valid local REST configuration
is still required. Enabled startup retains the API health check and loopback
fallback. `--enable-network-migration` enables dispatch and the independent lease;
infrastructure requests first run start/resume checks, then report
`ActionNotImplemented` if their external action handler is not yet wired. This flag does not implement the vCenter FSS/HTTP authorization gate.
Startup does not load NSX configuration or contact NSX. Infrastructure prerequisite
reconciliation lazily loads the mounted configuration and reads the selected NSX
Project; missing configuration or client initialization failures are retryable. Migration startup must not
rely solely on `HasVPCNamespaces()`, since a VDS Supervisor may have none.

Both CRDs accept `Evaluate`, `Plan`, `Derive`, and `Execute`. Namespace batches
additionally support `Rollback` and `Commit`; infrastructure supports `Finalize`.
The infrastructure CRD rejects `Rollback` at every phase. Failed infrastructure
actions retain checkpoints for diagnosis and forward retry. Infrastructure
reversion uses the user-operated WCP backup-and-restore procedure outside the
migration API; arrange a usable backup and recovery procedure before migration.
Namespace rollback remains subject to dependency checks and the Commit boundary.
`lastAction`, `actionState`, and `actionGeneration` record accepted work. A handler
returns `CheckpointIrreversible` only after prerequisite checks, then waits for
persisted `status.irreversibleAction` before irreversible external changes. That
checkpoint is immutable and closes namespace rollback at Commit.
Infrastructure Rollback is rejected independently of any irreversible checkpoint. Exact workflow IDs remain durable.
Namespace status contains `name`, `phase`, `reason`, and `message`; milestones are batch
conditions, and counters are derived using batch membership from spec.

## Design contract

The reviewed API uses `migration.network.vmware.com/v1alpha1` and
`spec.networkConfiguration`. DHCP inputs are `namespaceDHCPNetworks` or
`controlPlaneAndServiceDHCPNetworks`, both lists of `DHCPNetworkConfiguration`
keyed by `vdnName`. Workflow records also use `vdnName`. Action values use PascalCase
(`Evaluate`, `Plan`, `Derive`, `Execute`, `Rollback`, `Commit`, `Finalize`).
These are breaking changes to the pre-release API. Clients and dependent branches
must adopt them together; no aliases or conversion webhook are provided.

The orchestration API follows [the detailed design, version 158](https://vmw-confluence.broadcom.net/pages/viewpage.action?pageId=2586034708),
sections 4.1 and 4.2, and [WBS version 66](https://vmw-confluence.broadcom.net/pages/viewpage.action?pageId=3003298182).
The `design-*.yaml` fixtures reproduce the updated design examples and are
validated against the generated schemas. Proposed recovery extensions and
concrete handler behavior are explicitly distinguished from this framework.

In 9.2, supported migrations use HAProxy or FLB (`VSPHERE_FOUNDATION`) sources
and an NSX LB (`NSX_LB`) target. The API rejects unsupported provider values
and exposes no Avi configuration. Evaluate handlers must report unsupported
discovered sources through failure conditions before target preparation. The
dispatcher does not yet implement provider evaluation handlers. HAProxy requires `networkConfiguration.legacyLbFrontendDvpgs`; FLB
discovers frontend networks and VIP ranges. Unresolved FLB configuration is a
discovery failure, not a prompt to guess network values.

Control plane inputs use `networkConfiguration.controlPlaneAndServiceDHCPNetworks`;
namespace inputs use `networkConfiguration.namespaceDHCPNetworks`. Both contain one entry per `vdnName`,
with `dhcpRange.start`, `dhcpRange.end`, `subnetMask`, and `gatewayAddress`.
The inspection handler derives the subnet CIDR from the range start and mask,
then validates the range end and gateway against that subnet; no separate CIDR
input is exposed. Both network lists use `DHCPNetworkConfiguration` to describe
existing source networks, not to request new DHCP configuration.

While the requested action is `Evaluate` and no planning checkpoint exists,
network configuration may be added, corrected, or removed, including individual
addressing values, network entries, HAProxy frontend port groups, or the whole
configuration. Syntax validation still applies. Edits change the CR generation
and require fresh evaluation. Configuration cannot change in the same update
that requests Plan, or while Plan is pending and `status.lastAction` still
records Evaluate. The same request-time guard applies
to control-plane project/provider selection. The existing checkpoint-based
freeze remains, with one namespace-only exception: `networkConfiguration` may
change with requested action `Evaluate` when the stored `status.lastAction` is
`Rollback`, `status.actionState` is `Succeeded`, and no `irreversibleAction`
checkpoint exists. The action and inputs may change together or in separate
updates. Running, failed, or incomplete rollback checkpoints do not reopen edits.
Syntax validation and the requirement for fresh evaluation still apply; edits
cannot accompany a request for Plan. Infrastructure has no rollback exception.
Discovery-dependent checks, including subnet membership, ordered DHCP
bounds, unknown VDNs and conflicts between batches, belong to the concrete
handlers, which have not shipped.

`status.nsxWorkflows` is a map list keyed by `vdnName`, retaining the exact
`workflowId` for recovery. Infrastructure execution reports `InfraTransitionComplete`
and the matching condition; namespace execution reports `Completed`. Infrastructure
finalization reports `Finalized`; namespace commit reports `Committed`.
The checked-in `design-*.yaml` test fixtures retain all five current design output
examples. Apply the samples under `build/yaml/samples/migration` to begin with
`Evaluate`, then request each subsequent action after its prerequisites succeed.

For an NSX_LB target, `networkConfiguration.lbSnatIPv4` and `networkConfiguration.lbSnatIPv6`
optionally supply backend fallback addresses when the corresponding VIP pool
cannot provide a reservation during Evaluate. Each field accepts one plain IP
of its declared family; neither is required for the other family. They may be
added, corrected, or removed during Evaluate until the respective family has an
accepted reservation. Requesting Plan freezes all inputs even before its
checkpoint is recorded. See
`build/yaml/samples/migration/controlplane-nsx-lb.yaml`.
The CRD validates syntax and family; availability checks, early reservation,
reservation ownership, and cancellation cleanup require execution handlers.
Before recording `status.lbSnatReservations`, a handler must confirm the source
allocation's ownership, readiness, actual IP, and namespace/name. The accepted
checkpoint freezes that family's fallback input, provider selection, and project.
Checkpoint identity cannot be replaced or cleared. The unreserved address family
can still be supplied during Evaluate. These records do not allocate addresses;
post-planning revision/reset handling remains separate service work. Infrastructure has
no rollback-based reset path.

The design explicitly marks post-planning input revisions/reset, SNAT fallback confirmation,
resolved inventories, fabric provenance, rollback domains, cutover protection,
operation checkpoints and detailed commit/approval recovery records as proposed
extensions requiring finalized schemas and concrete handlers. This API alignment
does not implement those execution paths or remove the irreversible checkpoint
guards. The namespace CRD now permits corrected inputs during Evaluate after a
stored successful rollback. Before recording that success, the concrete rollback
handler must verify restoration of the whole batch and invalidate the previous
plan and derived results. That handler work remains separate; the schema
exception does not perform rollback or verify external state.

Evaluate-to-Plan freshness belongs to admission work WBS 1.1.4.1. For that action
change, require the old object's successful Evaluate checkpoint to have
`status.actionGeneration == oldObject.metadata.generation`. `observedGeneration`
alone is not proof of success; the new generation changes with the Plan request.
This separately tracked admission integration remains pending. No
`evaluatedSpecHash` field is added.

VCFA namespace onboarding and project selection belong to the VCFA client
workstream: every namespace must be imported before confirmation/Commit, using
an explicitly selected project or a find-or-create project named after the
namespace. These mappings are not additional transition CR inputs. VC-only
migration uses the same namespace transition API without VCFA onboarding.

## WBS 1.4.3 fabric preparation

`pkg/migration/fabric.Service.Ensure` prepares one resource in dependency order
under a `recovery.Session`. The caller supplies inspected DVS/transport-zone data
and reviewed topology. The NSX adapter in `pkg/nsx/services/migration/fabric.go`
uses typed Policy clients for TGWs, VPCs, infra IPBlocks, DVCs and VPC connectivity
profiles. DVC requests require a resolved VLAN, zone/span availability and the
SDK value `ENABLED_L2_AND_L3` (the design prose calls this L2/L3 extension).
Evaluate container preparation allows TGWs/VPCs; DVCs, ordinary blocks
and connectivity profiles require Derive. The dedicated SNAT sequence is separate.

Creation uses stable IDs and ownership tags; existing resources require matching
ownership or an explicit adoption unique ID and compatible configuration. Adopted
resources are never modified. Owned configuration updates preserve unmanaged SDK
fields and carry the observed revision into PUT. Policy errors are not absence,
and successful submission is not readiness: realization must be observed. Explicitly empty
managed configuration lists request empty configuration. Omitted (nil) lists and
optional scalar fields retain NSX defaults/current values, including TGW transit
subnets supplied by NSX.

Journals retain creation/adoption intent, payload, consumers and NSX unique IDs,
including identities observed before realization completes. Replaced/deleted
recorded resources block recovery instead of being recreated. Every reuse checks
live readiness even when the effect journal already says Applied. Consumer records
are retained per operation; cleanup must consult all journals and live consumers,
including other transition CRs. This service does not delete shared resources.

`NetOperatorSource.Reserve` uses the installed Net Operator API: `poolRef` references
a namespaced **Network**, not an IPPool. It creates a deterministic source allocation
in `vmware-system-netop`, retains its UID in the private journal, and verifies
ownership, Ready, exact requested IP and family. The retention finalizer
`migration.network.vmware.com/lb-snat` and absence of a transition ownerReference
prevent ordinary CR pruning from releasing SNAT. WBS 1.4.10 must implement ordered
retirement before removing that finalizer. The source allocator does not delete,
replace or mutate an unresolved request to implement fallback. Source cancellation
and fallback protection remain required integration contracts.

`SNATService.Bootstrap` checks supported capabilities and LB VPC readiness before
reservation. It journals the accepted source identity and persists
`status.lbSnatReservations` before target configuration. Status-write failure
resumes from the journal without allocating again. The backend must preserve the
source reservation and VIP exclusion, create the dedicated single-address block,
verify an exact target allocation, and configure/verify LB SNAT. It must preserve
the other address family. `PartitionVIPRanges` splits IPv4/IPv6 ranges around all
accepted SNAT addresses, avoiding overlapping broad frontend blocks. This arithmetic
alone neither updates source IPAM nor proves NSX block compatibility.

`SNATService.Verify` is the read-only reuse path for Derive and Stage 2. It does not
allocate another SNAT address or repair missing resources. Backend readiness is
not a replacement for pre-cutover dataplane/traffic validation.

### Integration gates still required

- Installed Net Operator `IPAddressAllocation` exposes a Network reference and
  optional exact IP, but no explicit family/pool selector. Source validation must
  prove selection of the intended VIP/backend pool and family, source-IPAM
  preservation across backend migration, and fallback cancellation/protection.
- `CannotAllocIP` is not proof of exhaustion. Only an authoritative exhaustion
  result may produce the family-specific `requiredInputs` entry. Transport, auth,
  realization and generic allocation failures remain errors.
- The installed SDK's `LBService` model has no explicit SNAT address property.
  Existing VPC restore code uses special service-allocation identities, but that
  alone does not establish the migration bootstrap contract for both TEP/TEPless.
  A verified target adapter must implement singleton-block compatibility, exact
  target allocation and LB SNAT readiness; none is invented or enabled here.
- Source inspection and approved Plan synthesis must provide VLAN/zone identities,
  topology dependencies, full managed configuration and consumer UIDs. VPC/TGW
  attachments and workflow conversion remain coordinated with their owning services.

The source-IPAM Role grants only Network reads and allocation get/create in
`vmware-system-netop`, bound to the NCP ServiceAccount in `vmware-system-nsx`.
The RBAC kustomization preserves each resource's explicit namespace. No new CRD
fields or normal networking-controller behavior are introduced.

### Opt-in live NSX harness

`pkg/nsx/services/migration/fabric_integration_test.go` runs real NSX VPC/TGW
create-and-realize, no-write reuse, explicit adoption and foreign-owner rejection
checks. It reconstructs a **local fake Kubernetes client** from serialized recovery
journals to simulate service restart. It does not test Kubernetes API-server CAS,
a real controller restart, Net Operator allocation, DVC conversion, SNAT or traffic.

Download the Jenkins `testbedInfo.json` to a private local file (mode 0600). The
harness reads `nsxmanager["1"]` credentials from that file; do not put credentials
in the command or commit the artifact. The disposable lab connection permits its
self-signed TLS certificate. Normal unit-test runs skip this test.

```sh
NSX_FABRIC_TESTBED=/private/tmp/testbedInfo.json \
NSX_FABRIC_ALLOW_CREATE=1 \
NSX_FABRIC_REPORT=/private/tmp/fabric-live-report.json \
go test ./pkg/nsx/services/migration \
  -run '^TestFabricNSXIntegration$' -count=1 -v -timeout=8m
```

The default project is `default` (`NSX_FABRIC_PROJECT` overrides it); it must have
an existing `default` TGW whose span can be read. Resources use a random `wbs143-`
ID and ownership tag. Cleanup checks ownership and recorded unique identity,
uses nonrecursive deletion, and confirms absence. Nothing is attached to the test
VPC/TGW. The report records test paths before mutation and cleanup results. If the
process is forcibly killed, inspect those exact paths and ownership before cleanup.

The first testbed-138 run exposed a TGW defaulting bug: NSX populated omitted
`transit_subnets`, which caused repeated updates. The adapter now preserves nil
lists, while explicitly empty lists still request removal. A regression test covers
that distinction. The subsequent live run passed all checks for VPC and TGW with
one write each, and confirmed both resources absent after cleanup. This result
validates those NSX resource operations, not full migration/SNAT readiness.

## WBS 1.2.1 infrastructure start and resume

`pkg/migration/infrastructure` runs before infrastructure action dispatch and is
rechecked under durable ownership before any registered handler is invoked.
It reports `InfrastructurePrerequisitesReady` with the current CR generation.
Missing prerequisites produce `EvaluationFailed` for Evaluate or `Blocked` for
later actions, with a concrete reason and a retry. Failed reads produce an Unknown
prerequisite condition, rather than claiming the provider/project is absent.
Existing action, workflow, VPC and SNAT checkpoints remain intact.

Provider validation reads the typed net-operator
`WorkloadNetworkConfiguration/default` through the uncached API reader. It requires
one VPC provider with a system configuration selecting the same project as the
transition, plus a Ready condition for the current configuration generation. The
active system provider must remain VDS for a new migration. An existing Execute
or Finalize checkpoint (including an Execute journal after a failed status write)
permits resumption when the active provider has already changed to VPC.

This consumes the platform-published registration contract; it does not call the
vCenter POST /providers endpoint or install NCP. Provider publication is the
platform/net-operator dependency tracked under WBS 2.4/8.3. A platform that has
installed NCP but has not yet published the VPC provider remains blocked. The
net-operator API dependency is pinned to the same published version used by the
network-inspection branch; no locally invented WLNC spec is used.

NCP readiness requires exactly one non-deleting Pod labelled
`nsx-operator-role=master` in `vmware-system-nsx`, with a running, ready
`nsx-operator` container and a container identity. The normal NSX operator sets
that role after controller registration. Migration sidecar readiness, aggregate
Pod readiness, or the existence of VPC namespaces is insufficient. This is a
Kubernetes readiness observation, not proof of dataplane health; live action
handlers retain their own external health and connectivity gates.

Project selection follows the design: omitted `spec.nsxProjectId` means
`/orgs/default/projects/default`; an explicit ID means
`/orgs/default/projects/<id>`. Paths/URLs are not accepted as IDs. The NSX adapter
uses the shared lazy client, requires `enable_vpc_network`, and performs a Project
GET only. Missing projects, authentication failures and transport errors never
trigger project creation or fallback. The normal operator's `default_project`
configuration does not override this migration API contract.

Before handler effects, the runtime records an immutable `infrastructure-start`
checkpoint containing the resolved project path, ID and NSX unique ID. Retries
reuse that binding and all existing workflow/container/allocation identities.
A deleted/recreated project or a change after recorded effects blocks recovery.
Existing resource checkpoints without a durable project binding require verified
adoption before resumption; the starter does not invent their ownership.
A previously omitted project may still be supplied during Evaluate if only
unused discovery records exist; this does not authorize reinterpreting prepared
resources. Historical journals are retained. No new user-facing CRD fields are
introduced, and these checks never mark Evaluate successful or prepare resources.
WBS 1.2.2 and the fabric/SNAT services must complete those operations.

Tests cover stale/missing provider publication, unready/standby/sidecar-only NCP,
project lookup failures without fallback, default/custom selection, resumed VPC
state, project replacement, recorded-effect conflicts, correction before effects,
and binding reuse after restart. Validation uses fake Kubernetes clients and NSX
Project responses; no live migration has been run for this change.

## WBS 1.4.9 action, locking and recovery runtime

`pkg/migration/recovery` stores versioned private ConfigMap records in
`vmware-system-nsx`. No additional transition CRD fields are introduced.

- `supervisor-network-migration-lock` is the single authoritative record for both
  mutation ownership and `admissionProtectionActive`. It records the holder CR
  UID, kind/name, action/generation, fencing counter, namespace UID scope, and
  checkpoint. There is no TTL takeover. A lock alone does not activate protection.
- The current implementation serializes all Supervisor migration mutations. This
  is deliberately more conservative than independent DVS/VPC locks and prevents
  shared topology effects from overlapping. Lock contention remains retryable.
- `migration-op-<identity hash>` journals preserve immutable named recovery
  checkpoints (such as original backing and resource provenance), external-effect
  intents, outcomes, verified completion, and confirmation of persisted CR status.
  Corrected Evaluate and namespace rollback retain previous journals; handlers can
  inspect them through `Execution.History`. Journals have no owner-reference GC or
  delete permission. Retention/cleanup policy is separate work.
- A recovery finalizer is persisted before handler execution. Deletion with an
  active holder resumes the recorded recovery instead of releasing protection or
  erasing evidence. Successful completion releases ownership before removal of
  the finalizer on a subsequent deletion reconciliation.

The controller acquires/reuses durable ownership and rechecks dependency gates
before invoking a registered handler. Preparing/Evaluate, Plan and Derive keep
admission protection inactive. Execute activates protection first. Namespace
rollback activates or retains it if any Execute operation was recorded, including
an uncertain attempt; preparation-only rollback leaves it inactive. Infrastructure
scope currently conservatively includes all namespaces; namespace batches use
resolved namespace UIDs. Active scope cannot be changed during recovery.

Production handlers receive `Request.Execution`. Before an external mutation,
record immutable recovery data with `Checkpoint` and run the mutation through
`Effect`, supplying a stable target identity/payload digest, an authoritative
probe, and an idempotent apply callback. The journal stores intent before apply.
Retries probe first: Applied skips submission, Pending/unknown/errors retain the
holder and do not authorize another call, and authoritative Absent permits apply
with the same idempotency key. Before submission the runtime rechecks the current
CR UID/action/generation and, for Commit/Finalize, the persisted irreversible
checkpoint. An existing foreign resource must be reported as a conflict, never
adopted merely because its name matches.

Fencing checks reject stale sessions at journal/effect boundaries, and resource
version preconditions reject conflicting lock updates. They cannot revoke an
already in-flight external request: handlers must honor manager cancellation and
use external idempotency/ownership reconciliation. This framework does not claim
exactly-once NSX/vCenter execution. Any service lacking those guarantees must
leave an ambiguous outcome pending for reconciliation, rather than blindly retry.

A handler may return Succeeded only after verifying the full action's consistent
state (including whole-batch restoration and plan invalidation for Rollback).
The engine rejects unresolved journal effects, persists verification, and then
the controller persists CR success. Release proceeds as separate recoverable
writes: record completion, clear protection, release holder. Failure, NeedsInput,
timeout, or restart does not clear protection. If CR status persistence fails after
verification, restart restores the complete handler-produced status snapshot
(including workflow and allocation identities) and publishes success without
repeating the handler. A client
requesting the next action before release cannot strand the old holder.

`ConfirmProtection` is an explicit integration point for WBS 1.1.4.3. It must
confirm that deployed guards enforce the exact authoritative holder and scope;
merely seeing a webhook configuration is insufficient. Admission consumers must
read the authoritative lock with an uncached reader and fail closed on an
unavailable/corrupt record. They must not infer protection from CR phase or a
status mirror. `ConfirmFreshnessAdmission` confirms the separately tracked WBS
1.1.4.1 deployment before Plan handlers run. Both callbacks are intentionally
unset until their admission implementation is wired: Plan and protected execution
fail closed, without external handler invocation. These hooks do not implement
admission request validation themselves.

The existing WBS 1.1.6 protections are also included: handlers cannot assign or
clear `irreversibleAction`, completed Commit/Finalize records without a marker are
rejected, and namespace terminal shortcuts require the Commit marker.
Concrete external action handlers remain unregistered. Unit/fault-injection tests
cover the runtime contract; end-to-end completion of WBS 1.4.9 still requires
admission and concrete handler integration and deployment verification.

## Build and validate

```sh
make generate-migration manifests-migration
make test-migration
make build-migration
make docker-build-migration MIGRATION_IMG=supervisor-network-migration-operator:latest
```

Use the repository Go toolchain and Kubernetes/controller-runtime versions. CRD
admission is validated with the repository-pinned Kubernetes 1.35 API-server
libraries and their standard CEL compatibility settings. The API module has its
own admission/serialization tests, run by `test-migration-api` in CI
and included in `test-migration`.
The existing NSX operator and coverage images contain only their own binaries.
Build the separate image with `make docker-build-migration`; its Dockerfile is
`build/image/migration/Dockerfile`. Its entrypoint runs
`/usr/local/bin/supervisor-network-migration-operator`. The existing NSX operator
images retain their manager entrypoint.

## NCP packaging integration

The production NCP Deployment is supplied outside this repository.
`build/yaml/migration/ncp-container.patch.yaml` is a strategic merge integration
patch, not a standalone Deployment. The packaging owner must:

1. Install the migration CRDs and `build/yaml/migration/rbac` before starting the
   new container. Match both bindings to the actual NCP Pod ServiceAccount
   (`nsx-ncp` is the checked-in default); the Pod shares one ServiceAccount.
2. Merge the container patch into the actual NCP Pod template and replace its
   image placeholder with the pinned image containing the migration binary.
3. Mount the existing NSX operator config at `/etc/nsx-operator/nsxop.ini`, along
   with its required CA, authentication/token mounts, and Envoy settings. Reuse
   those supported mounts rather than copying credentials into a new ConfigMap.
4. Keep port 9924 distinct from other host-network listeners. Metrics default to
   disabled. Migration liveness/readiness describe the process and do not depend
   on NSX health or aggregate NCP Pod readiness. Check the NSX operator container
   and its controller state explicitly when implementing migration prerequisites.
5. When migration is enabled, preserve an independent lease named
   `supervisor-network-migration-operator.iaas.vmware.com` in `vmware-system-nsx`.
   The migration and NSX leaders may be in different Pods; communicate via APIs
   and CRs, not assumptions about colocated leadership or shared memory.

## Handoff and remaining work

Stop the standalone tera migration controller before enabling this replacement.
Preserve existing transition CRs and explicitly transfer CRD/package ownership;
removing the old Carvel package must not delete the CRDs or their instances.
The old tera bootstrapper/build wiring is removed as part of that coordinated
packaging change, not by this repository alone.

The WBS 1.4.9 runtime supplies durable mutation ownership, recovery finalizers,
operation journals, and admission-protection coordination. Concrete handlers still
need current NSX ownership/consumer checks, reviewed plan verification, resource
provenance capture, and approval/onboarding checks. Admission webhooks remain the
separate WBS 1.1.4 integration. Dependency gates are repeated under durable ownership;
handlers must also recheck live external dependencies before each effect.
Never duplicate normal NSX controllers or delete active VPC/LB/SNAT resources.
`NetworkResourceTransition` support is a prerequisite for real cutover handlers;
it is not implemented by this scaffold. Container co-location does not replace
these recovery contracts or protect against Pod replacement.

The two orchestration CRDs use `migration.network.vmware.com/v1alpha1`; resource transfer
uses the separate `NetworkResourceTransition` API and remains an NSX Operator
responsibility. The leader-election
lease keeps its historical name across the API group change.
