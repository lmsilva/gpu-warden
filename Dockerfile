# Build all three binaries, then ship them on a base with nothing else in it.
#
# The Go version is pinned rather than tracking latest, so an image built six
# months from now is the same image.
FROM golang:1.26.5 AS build
WORKDIR /src

# Dependencies first, as their own layer: they change far less often than the
# code, so an edit to a .go file does not re-download the module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# The version to stamp into both binaries. Defaults to "dev" so a local build
# is honest about not being a release; CI passes the git tag.
#
# It is an argument rather than being read from git because .dockerignore
# excludes .git - the build context has no repository to ask.
ARG VERSION=dev

# CGO off gives a static binary, which is what lets the final stage be
# distroless. It is on by default and would link against a libc the runtime
# image does not have.
#
# Symbol tables and DWARF stripped: nothing debugs a container by attaching
# gdb to it, and they are a third of the size.
ENV CGO_ENABLED=0
RUN LDFLAGS="-s -w -X github.com/lmsilva/squire/internal/buildinfo.Version=${VERSION}" \
 && go build -trimpath -ldflags="$LDFLAGS" -o /out/squire      ./cmd/squire \
 && go build -trimpath -ldflags="$LDFLAGS" -o /out/squire-lint ./cmd/squire-lint \
 && go build -trimpath -ldflags="$LDFLAGS" -o /out/squire-me   ./cmd/squire-me

# Distroless static rather than scratch: it carries CA certificates, without
# which every HTTPS call to slurmrestd or the Kubernetes API fails with an
# unknown-authority error that looks like a cluster misconfiguration.
#
# The :nonroot tag runs as uid 65532. Squire writes nothing to disk and needs
# no capabilities, so there is nothing for root to be needed for.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/squire      /usr/local/bin/squire
COPY --from=build /out/squire-lint /usr/local/bin/squire-lint
COPY --from=build /out/squire-me   /usr/local/bin/squire-me

# The licence terms travel with the binaries, not just with the source.
COPY LICENSE NOTICE /usr/local/share/squire/

USER 65532:65532
EXPOSE 9101
ENTRYPOINT ["/usr/local/bin/squire"]
