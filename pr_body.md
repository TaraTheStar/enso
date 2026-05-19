## Summary

- Decouples the content-addressed execution path from the stable mount root in `exestage`.
- Updates the Lima backend to mount the stable root instead of the specific binary directory, preventing VM drift-recreate cycles on host rebuilds.
- Adds an Alpine-specific `bootSpeedupScript` to eliminate the ~10s GRUB timeout during cold boots.
- Refines the Lima provisioning sequence to ensure boot speedup and iptables bootstrap occur in a deterministic, non-blocking order.

## Motivation

Currently, every time the `enso` binary is rebuilt, its content-addressed hash changes. Because the Lima backend was mounting the specific directory containing that hash, the VM configuration YAML would "drift," triggering a full, expensive (~10s) cold boot and re-provisioning cycle. 

By mounting the parent `exe/` directory (the stable root) and executing the specific hash-prefixed path within it, we maintain a constant mount point. This allows persistent VMs to stay running across host rebuilds, while still providing the safety of immutable, content-addressed binaries.

## Changes

- **`internal/backend/exestage/exestage.go`**: Refactored `Stage` to return both the `execPath` and the `root` (the stable mount point).
- **`internal/backend/lima/lima.go`**: 
    - Updated `buildVMConfig` and `ensureRunning` to use the stable mount root.
    - Implemented `bootSpeedupScript` to silence GRUB/extlinux timeouts on Alpine guests.
    - Re-ordered provisioning steps: `[Speedup] -> [iptables] -> [User Init]`.
- **`internal/backend/podman/podman.go`**: Updated to handle the new `Stage` signature.
- **Tests**: Updated `args_test.go`, `exestage_test.go`, `egress_e2e_test.go`, and `overlay_e2e_test.go` to validate the stability of the mount root and the correctness of the new provisioning logic.

## Test Plan

- [x] Unit tests for `exestage` confirm the mount root is invariant across rebuilds.
- [x] `lima/args_test.go` verifies the YAML configuration uses the stable root and correct provisioning order.
- [x] E2E tests for Lima and Podman confirm successful execution using the new staging model.
- [x] Verified that the `bootSpeedupScript` is applied correctly in Alpine-specific scenarios.

## Notes for Reviewers

The most critical logic change is in `internal/backend/lima/lima.go` regarding how `buildVMConfig` constructs the mount points. This is the mechanism that prevents the "drift-recreate" loop. The `bootSpeedupScript` is purely cosmetic and designed to be highly resilient (it won't fail a boot if it fails to run).
