bin_filename := "custos"

# Container image
IMG_TAG := "latest"
GHCR_IMG := "ghcr.io/helmetica-framework/custos:" + IMG_TAG

# The tag `just load-image` builds under. Anything but latest: Kubernetes
# defaults imagePullPolicy to Always for latest, so the kubelet would go to the
# registry for it and ignore the image sitting on the node.
DEV_IMG_TAG := "dev"
DEV_IMG := "ghcr.io/helmetica-framework/custos:" + DEV_IMG_TAG

# The local dev cluster from athanor's `just ignite`. Kind is pinned rather
# than taken off PATH because an older kind cannot read the node image's
# containerd config and its loader fails.
# renovate: datasource=github-releases depName=kubernetes-sigs/kind
KIND_VERSION := "v0.32.0"
KIND_CMD := "go run sigs.k8s.io/kind@" + KIND_VERSION
KIND_CLUSTER := "athanor"

# renovate: datasource=go depName=github.com/kyverno/chainsaw
CHAINSAW_VERSION := "v0.2.15"
CHAINSAW_CMD := "go run github.com/kyverno/chainsaw@" + CHAINSAW_VERSION

# Tools, pinned by the tool directives in go.mod
CONTROLLER_GEN := "go tool sigs.k8s.io/controller-tools/cmd/controller-gen"
KUSTOMIZE := "go tool sigs.k8s.io/kustomize/kustomize/v5"
