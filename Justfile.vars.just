bin_filename := "custos"

# Container image
IMG_TAG := "latest"
GHCR_IMG := "ghcr.io/helmetica-framework/custos:" + IMG_TAG

# Tools, pinned by the tool directives in go.mod
CONTROLLER_GEN := "go tool sigs.k8s.io/controller-tools/cmd/controller-gen"
KUSTOMIZE := "go tool sigs.k8s.io/kustomize/kustomize/v5"
