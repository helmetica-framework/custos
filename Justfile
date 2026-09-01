import "Justfile.vars.just"

export GOEXPERIMENT := "jsonv2"

_default:
    @just --list

# Build the manager binary, generators and checks included
build: generate manifests fmt vet binary

# CGO is disabled here only, not globally: the image needs a static binary,
# while `just test` runs with -race, which requires cgo.
#
# Build the binary without running the generators
binary:
    @echo "GOOS=$(go env GOOS) GOARCH=$(go env GOARCH)"
    CGO_ENABLED=0 go build -o {{ bin_filename }}

# Run tests
test: manifests generate
    go test ./... -race -coverprofile cover.tmp.out
    grep -v -e "zz_generated.deepcopy.go" -e "/applyconfiguration/" cover.tmp.out > cover.out

# Generate ClusterRole and CustomResourceDefinition objects
manifests:
    {{ CONTROLLER_GEN }} rbac:roleName=manager-role crd:generateEmbeddedObjectMeta=true paths="./..." output:crd:artifacts:config=config/crd/bases

# Generate deepcopy functions, apply configurations and manifests
generate: manifests
    go generate ./...
    {{ CONTROLLER_GEN }} object paths="./..."
    {{ CONTROLLER_GEN }} applyconfiguration paths="./api/..."

# Generate documentation
docs:
    @echo "Nothing to do yet"

# Run go fmt against code
fmt:
    go fmt ./...

# Run go vet against code
vet:
    go vet ./...

# All-in-one linting
lint: fmt vet generate manifests docs
    @echo 'Checking kustomize build ...'
    {{ KUSTOMIZE }} build config/crd -o /dev/null
    {{ KUSTOMIZE }} build config/default -o /dev/null
    @echo 'Check for uncommitted changes ...'
    git diff --exit-code

# Build the docker image
build-docker: binary
    docker build . --tag {{ GHCR_IMG }}

# Build the image and put it on the local cluster's nodes
load-image: binary
    docker build . --tag {{ DEV_IMG }}
    {{ KIND_CMD }} load docker-image {{ DEV_IMG }} --name {{ KIND_CLUSTER }}

# Run the controller from your host
run: manifests generate fmt vet load-image
    go run main.go controller --gather-image {{ DEV_IMG }}

# Needs a running athanor cluster (just ignite). The suite installs the CRDs
# and the manager itself; load-image is a dependency because the gather Job
# runs the same image and a bare kind cluster cannot pull the published one.

# Read custos's purity: the end-to-end test
touchstone: load-image load-image
    {{ CHAINSAW_CMD }} test --config test/touchstone/chainsaw-config.yaml test/touchstone

# Clean up the generated resources
clean:
    rm -rf contrib/completion dist/ cover.out cover.tmp.out {{ bin_filename }} || true
