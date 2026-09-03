# Custos

**Custos**: the guardian, the one an alchemist trusts to keep the arcanum
and let no one at it who has no business with it.

Custos handles credentials and secret management for services. It provides the access credential for the services to
the end-user.

## Quickstart

Against a kind (or any) cluster:

```bash
kubectl apply -k config/crd
just run          # in a second terminal
kubectl apply -k config/samples
kubectl get arcana -w   # PHASE -> Ready
```

The sample creates an `Arcanum` named `sample`. The controller settles it to
`Ready` and stamps the generation it settled on.

Set `spec.suspend: true` to pause it. A suspended arcanum holds at `Pending`
and custos leaves everything it owns alone, which is the escape hatch when
the controller and the cluster disagree. Clearing the field releases it.

**`config/default` deploys RBAC and the manager, but not the CRDs.** Install
the CRDs separately from `config/crd` first:

```bash
kubectl apply -k config/crd
kubectl apply -k config/default
```

## Structure

| Path | What's there |
| ---- | ------------- |
| `api/v1` | The `Arcanum` type (`arcana.helmetica.io/v1`): spec, status and generated deepcopy code. |
| `cmd` | The `custos` CLI (cobra): `root.go` wires the binary, `controller.go` is the `custos controller` subcommand that builds the manager and registers `ArcanumManager`. |
| `controllers` | `ArcanumManager`, the reconciler for `Arcanum`. `desiredPhase` is the seam the real logic goes behind. |
| `config` | Kustomize tree: `crd` (the CRD manifest, installed separately), `rbac`, `manager`, `default` (RBAC + manager, no CRDs) and `samples`. |

## Growing it

`desiredPhase` returns the phase and message the status should carry, and
may return an error to requeue. Anything custos provisions goes there,
behind the suspend check. When it starts owning cluster objects, add the
matching `+kubebuilder:rbac` markers next to `Reconcile` and an `Owns(...)`
watch in `SetupWithManager`, then re-run `just generate`.
