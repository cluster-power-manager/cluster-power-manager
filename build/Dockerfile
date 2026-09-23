ARG BUILDER_IMAGE=quay.io/projectquay/golang:1.24
ARG RUNTIME_IMAGE=scratch
ARG STRICTFIPS="false"

# Build the manager binary
FROM --platform=$BUILDPLATFORM ${BUILDER_IMAGE} AS builder
ARG TARGETOS \
    TARGETARCH
ARG STRICTFIPS

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum

# Bring in the go dependencies before anything else so we can take
# advantage of caching these layers in future builds.
COPY vendor/ vendor/
# Copy the go source
COPY build/manager/main.go main.go
COPY api/ api/
COPY controllers/ controllers/
COPY pkg/ pkg/
COPY internal/ internal/

# Build
RUN if [[ "${STRICTFIPS}" == "true" ]]; then \
        GOEXPERIMENT=strictfipsruntime CGO_ENABLED=1 GO111MODULE=on GOOS=${TARGETOS:-linux} GOARCH=$TARGETARCH go build -tags strictfipsruntime -a -o manager main.go; \
    else \
        CGO_ENABLED=0 GO111MODULE=on GOOS=${TARGETOS:-linux} GOARCH=$TARGETARCH go build -a -o manager main.go; \
    fi

# Final image
FROM ${RUNTIME_IMAGE}
ARG MANIFEST=build/manifests/power-node-agent-ds.yaml
WORKDIR /
COPY LICENSE /licenses/LICENSE
COPY --from=builder /workspace/manager .
COPY ${MANIFEST} /power-manifests/
USER 10001

ENTRYPOINT ["/manager"]
