# Custos

**Custos**: the guardian, the one an alchemist trusts to keep the arcanum
and let no one at it who has no business with it.

An `Arcanum` says which values a service's credentials are made of and where
each one comes from. Custos collects them, renders them and writes one Secret
where the consumer can reach it.

## Quickstart

Against a kind (or any) cluster:

```bash
kubectl apply -k config/crd
just run          # in a second terminal
kubectl apply -k config/samples
kubectl get arcana -w   # PHASE -> Ready
kubectl get secret sample-credentials -o jsonpath='{.data.URL}' | base64 -d
```

The sample builds `postgres://sample-rw:5432/sample` out of two constants and
a template, which needs no cluster access at all and settles in one pass.

Set `spec.suspend: true` to pause it. A suspended arcanum holds at `Pending`
and custos leaves everything it owns alone, including a Secret it has already
written, which is the escape hatch when the controller and the cluster
disagree. Clearing the field releases it.

**`config/default` deploys RBAC and the manager, but not the CRDs.** Install
the CRDs separately from `config/crd` first:

```bash
kubectl apply -k config/crd
kubectl apply -k config/default
```

## The value mapping

`spec.credentials.valueMapping` is keyed by the name the value gets in the
Secret. Consumers read those names, so they are the chart author's to choose
and are never derived from wherever the value happened to come from.

| Source | Reads | Resolved by |
| ------ | ----- | ----------- |
| `const` | `value`, literally. | Custos. |
| `template` | `value` as a Go template over the other keys. | Custos, after its dependencies. |
| `claimParam` | `path`, a JSONPath into the claim CR. | Custos, through an uncached read. |
| `objectRef` | `key` or `path` of the `kind`/`name` object in the instance namespace. | The gather Job. |
| `exec` | stdout of `command` in a pod matching `podSelector`. | The gather Job. |

The last two are why there is a Job. Reading arbitrary kinds and running
commands in pods needs permissions custos itself must not hold, so it borrows
`instance-admin`, the ServiceAccount chrysopoeia already binds to `admin`
inside the instance namespace.

## Templating

Two layers, in this order.

**Metadata**, available in `name` and in `command` arguments, and to
templates. Six names, and nothing else:

`ClaimName`, `ClaimNamespace`, `ClaimKind`, `ClaimAPIVersion`,
`InstanceNamespace`, `ArcanumName`.

So `name: "{{.ClaimName}}-app"` points an `objectRef` at a Secret whose name
chryso derived from the claim.

**Templates**, which see the metadata and every other mapping key. Custos
orders them by what they reference, so a template can be built on another
template. A cycle or a reference to a name that does not exist is rejected
before anything is created, because an `exec` may provision and a spec that
can never render must not get that far.

## Where the Secret lands

Beside the claim when the arcanum's namespace carries chrysopoeia's
annotations, and in the instance namespace when it does not. The namespace is not
configurable: it follows from where the consumer is, and letting a chart
author pick would let one tenant write into another's namespace.

A cross-namespace Secret cannot carry an owner reference, so custos holds a
finalizer and removes the Secret itself when the arcanum goes.

Custos never adopts a Secret it did not create. If the target name is already
taken by something without its labels, the arcanum goes `Failed` and the
existing Secret is left exactly as it was.

## When a gather runs

On a changed plan, and on nothing else. The plan is hashed into the Job's
name, so an unchanged mapping finds the Job it already ran and skips the
stage. To force one, change the `custos.helmetica.io/refresh` annotation to
any new value; it is hashed in too. This matters because an `exec` is allowed
to provision, and a gather that re-ran every reconcile could mint a new
password every few seconds.

A failed gather reports the message its Job left in a ConfigMap rather than
its log, which is what keeps custos off `pods/log`, a cluster-wide grant over
output that routinely contains secrets.

## Structure

| Path | What's there |
| ---- | ------------- |
| `api/v1` | The `Arcanum` type (`arcana.helmetica.io/v1`): spec, status, the CEL rules that keep malformed sources out, and generated deepcopy code. |
| `cmd` | The `custos` CLI (cobra): `controller.go` builds the manager, `gather.go` is the `custos gather` subcommand the Job runs. |
| `controllers` | `ArcanumManager`. `desiredPhase` is the flow; `resolve.go` validates and renders, `gather.go` builds the plan and the Job, `target.go` writes and takes back the Secret. |
| `gather` | The plan the controller writes and the Job reads, plus the resolution the Job performs. Shared by both sides on purpose: a plan written one way and read another would only show up in a cluster. |
| `config` | Kustomize tree: `crd` (installed separately), `rbac`, `manager`, `default` (RBAC + manager, no CRDs) and `samples`. |
| `test/touchstone` | The chainsaw suite. `just touchstone` against a cluster with custos deployed. |

