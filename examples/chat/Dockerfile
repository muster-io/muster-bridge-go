# SPDX-License-Identifier: MIT
# Copyright The Muster Authors

# The image of the example bridge, built from the module's root:
#
#	docker build -f examples/chat/Dockerfile --build-arg VERSION=v0.1.0 -t muster-example-bridge .
#
# The build stage runs on the build machine's platform and cross-compiles, so that no platform is emulated. Both base
# images are pinned by the digest of their multi-platform index, which Renovate updates. Arguments given to docker run
# are appended to the ENTRYPOINT, so that -listen can be given again; the last one wins.
FROM --platform=$BUILDPLATFORM golang:1.27.2@sha256:820153096fda4de6ac1b7194d12e416b7dd8f4e9a33d17e0345b111b945162d8 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION
WORKDIR /src
COPY go.mod go.sum ./
RUN GOTOOLCHAIN=local go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOTOOLCHAIN=local \
	go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/example-bridge ./examples/chat

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/example-bridge /example-bridge
USER 65532:65532
EXPOSE 8095
ENTRYPOINT ["/example-bridge", "-listen", ":8095"]
